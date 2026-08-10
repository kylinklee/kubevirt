# Stop 流程写风暴：v1.2.0 vs v1.6.6 对比分析（2026-08-10）

> 本文档分析"Stop 后 VMI 自动拉起"根因链中的**写风暴环节**：两版本在 Stop 流程的
> 写入行为差异、写风暴的形成机制、以及为什么 v1.2.0 没有风暴。
> 配套文档：`docs/Stop后VMI自动拉起-根因分析总结-20260810.md`（最终根因链）。

---

## 1. 现象：v1.6.6 的写入密度（08:10 测试日志实测）

```
08:10:05.067  processing stop request（virt-controller vm 控制器）
08:10:05.105  vmi update event → execute: start（virt-handler）
08:10:05.135  Signaled graceful shutdown（第 1 次）
08:10:05.198  Updating the VirtualMachine status failed（409）
08:10:05.231  Updating the VirtualMachineInstance status failed（409）→ re-enqueue
08:10:05.235  Signaled（第 2 次）
08:10:05.248  Updating the VirtualMachine status failed（409）
08:10:05.261  Updating the VirtualMachineInstance status failed（409）
08:10:05.288/319/345/553  ...（反复 409 + re-enqueue + Signaled）
08:10:05-35   每 5s Signaled 重发 + 409 重试持续（65s 循环）
```

**特征**：
- **Stop 后 0.5s 内 14 次 reconcile**（事件风暴）
- **每 5s 一次 Signaled 重发**（AddAfter 5s，virt-handler shutdownVMI）
- **每次 reconcile 都尝试 Update VMI**（status 变化 → Update → 409 → re-enqueue）
- **virt-controller 与 virt-handler 同时写同一 VMI** → 409 竞争 → 双方重试

## 2. 两版本代码对比：写入逻辑相同，差异在事件链路

### virt-handler 侧（代码相同）

| 环节 | v1.2.0 | v1.6.6 | 差异 |
|------|--------|--------|------|
| shutdownVMI（Signaled + AddAfter） | AddAfter 5s（vm.go:2082） | AddAfter 5s（vm.go:1673） | 相同 |
| updateVMIStatus（每次 reconcile 必调） | 相同逻辑 | 相同逻辑 | 相同 |
| updateVMIStatusFromDomain 子函数 | updateIsoSizeStatus/updateSELinuxContext/updateGuestInfoFromDomain/updateVolumeStatusesFromDomain/updateFSFreezeStatus/updateMachineType/updateMemoryInfo/netStat | 完全相同清单 | 相同 |
| updateSELinuxContext | present→写 context / 否则 "none"（vm.go:1344） | 相同（vm.go:986） | 相同 |

### virt-controller 侧（代码相同）

| 环节 | v1.2.0 | v1.6.6 | 差异 |
|------|--------|--------|------|
| vm 控制器 stopRequest | "processing stop request"（vm.go:941） | 相同（vm.go:1020） | 相同 |
| vmi 控制器 updateStatus | vmi.go:522 | lifecycle.go:246 | 相同 |
| 控制器清单 | vm/vmi/rs/pool/migration...（application.go:542-549） | 相同结构（application.go:603-613） | 相同 |
| 线程数/QPS | DefaultVirtControllerQPS/Burst | 相同 | 相同 |

**结论：写风暴不是 v1.6.6 代码"新增写入"造成的——两版本的写入逻辑完全一致。**

## 3. ★ 写风暴的形成机制（v1.6.6）

```
初始触发（存储 watch 停摆，见根因文档）：
Stop → 存储 watch 推送停摆 → apiserver 无增量 → virt-handler 收不到 VMI 事件

放大器（正反馈循环）：
① expectations 卡住（Update 成功 → SetExpectations(1,0) → 事件不到 → 未清账）
② execute() 入口静默跳过所有 reconcile —— 但 AddAfter 5s 的 requeue 仍持续触发
   → 每次触发：Signaled 重发 + updateVMIStatus（status 变化 → Update 尝试）
③ Update 409（virt-controller 竞争）→ re-enqueue → 立即重试（AddRateLimited）
④ 写入量激增 → 存储压力更大 → watch 更不稳 → 停摆更长 → 更多循环

净效果：65s 内 virt-handler ~20-30 次 Update 尝试（正常流程的 3-4 倍）
+ virt-controller 恢复后的并发写（PATCH/PUT 422/404/409）
→ 并发写冲突（存储 row_count not zero）→ 写入失败（500）
```

## 4. 为什么 v1.2.0 没有写风暴

```
v1.2.0（事件正常）：Stop → 优雅关机 → shutoff（7s）→ domain 事件 → reconcile
→ 判 Succeeded（66ms）→ 流程结束

写入量：virt-handler 3-5 次 + virt-controller 2-4 次 ≈ 10 次内，~1s 内完成
→ 无 5s 循环（流程快速结束，不需要）、无持续 409 竞争（窗口短）
→ 存储压力小 → 不触发 watch 停摆 → 无正反馈
```

**关键差异**：
| 维度 | v1.2.0 | v1.6.6 |
|------|--------|--------|
| 事件到达 | 正常（毫秒级） | ★ 46s+ 不到（存储 watch 停摆） |
| Stop 流程时长 | ~1s（优雅关机后立即判 Succeeded） | 65s+（卡在 expectations） |
| virt-handler 写入 | 3-5 次 | ~20-30 次（5s 循环 + 409 重试） |
| 并发竞争窗口 | 短（流程快结束） | 长（65s 持续 + 恢复后 virt-controller 并发） |
| 存储压力 | 小 | ★ 大（触发并发写冲突 row_count not zero） |

## 5. 结论

1. **写风暴不是 v1.6.6 的代码缺陷**（两版本写入逻辑相同）——**是"事件链路异常"的结果放大器**
2. **正反馈循环**：事件不到 → expectations 卡 → 5s 循环写入 → 存储压力 → watch 更不稳
3. **v1.2.0 无风暴**：事件正常 → 流程快速结束 → 无循环 → 无压力
4. **修复意义**：
   - **打断正反馈**（减轻写风暴）：virt-handler 409 后不立即重试（等 AddAfter 5s 自然节奏）、vm 控制器 stopRequest 去重——**降低存储压力，缩短停摆恢复时间**
   - **兜底**（方向B）：virt-launcher 不超时退出——即使停摆发生也不重启
   - **治本**：存储侧修复 watch 停摆（厂商）

## 6. 附录：写入量估算依据

- virt-handler 5s 循环：AddAfter 5s（vm.go:1673 shutdownVMI）→ 65s ≈ 13 次 reconcile
- 每次 reconcile 的 updateVMIStatus：status 变化（conditions 等）→ Update 尝试 1 次
- 409 重试：AddRateLimited 指数退避（5ms→10ms→...→上限）→ 约 1-2 倍额外写入
- 总估算：13 × (1 + 1~2) ≈ 26-39 次 Update 尝试（v1.6.6 65s 窗口）
- v1.2.0 对照：优雅关机 7s + 判 Succeeded 66ms ≈ 5 次以内
