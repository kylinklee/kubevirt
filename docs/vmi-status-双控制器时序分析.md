# VMI 状态（phase）双控制器时序分析：virt-controller vs virt-handler

> 背景：Stop 虚拟机关机时 watch VMI 资源状态——
> **v1.2.0：Running → Succeeded（直接跳）**；**v1.6.6：Running → Failed（直接跳）**。
> 本文档解释"谁在什么时候写 VMI phase"、两个控制器的职责与时序竞态。

---

## 1. watch 观察差异的本质

| | v1.2.0 | v1.6.6 |
|--|--------|--------|
| phase 跳变 | Running → **Succeeded** | Running → **Failed** |
| 直接跳（无中间 phase） | ✅ | ✅ |
| **写入者** | **virt-handler**（domain 状态驱动） | **virt-controller**（pod 状态驱动） |
| 判定依据 | domain `shutoff:shutdown`（优雅关机成功）→ Succeeded | VMI `Running` + pod 不存在 → Failed |
| 写入方式 | Update（设 PhaseTransitionTimestamp） | JSON Patch（不设时间戳） |

**"直接跳"的原因**：两版本都是一步到位把 `phase` 字段改成终态（Succeeded / Failed），中间不经过其他 phase。

---

## 2. 双控制器职责划分（谁管 VMI 的什么）

| 维度 | virt-controller（vmi 控制器） | virt-handler |
|------|------------------------------|--------------|
| 负责阶段 | **前半段**：Unprocessed → Scheduling → Scheduled → Running；VMI final 后移除 finalizer | **后半段**：Running 中的 domain 生命周期、phase 终态判定（Succeeded/Failed） |
| 主函数 | v1.2.0: `updateStatus`（vmi.go:522）/ v1.6.6: `updateStatus`（lifecycle.go:246） | v1.2.0: `defaultExecute`（vm.go:1836）/ v1.6.6: `sync`（vm.go:1341） |
| phase 判定 | `setVmPhaseForStatusReason`：v1.2.0 vm.go:3241 / v1.6.6 vm.go:2264 | 根据 **pod** 状态（vmi.go:717 / lifecycle.go:416 等） |
| 终态来源 | **pod**：pod 不存在 → Failed；pod Succeeded + 迁移完成 → Succeeded | **domain**：shutoff:shutdown → Succeeded；domain==nil + 非 final → Failed |
| 更新方式 | Running/Scheduled → **JSON Patch**（不设 phaseTransitionTimestamp）；其他 → Update | **Update**（设 phaseTransitionTimestamp） |
| VMI final 后 | `IsFinal` 分支：allPodsDeleted → 移除 finalizer（vmi.go:697 / lifecycle.go:424） | 不再更新（updateVMIStatus 开头 `vmi.IsFinal() → return`） |

**一句话**：**virt-controller 管"VMI 从生到 Running"，virt-handler 管"Running 之后的生死判定"**——但两者都可能在 Running 阶段写 phase，形成竞态。

---

## 3. Stop 完整时序（两版本对照）

### 3.1 v1.2.0（正常：Running → Succeeded）

```
0s        v1.2.0 客户端 Stop → virt-api subresource /stop
          → virt-api：RerunOnFailure → patchVMStatusStopped（VM 加 StopRequested，保留 runStrategy）
          → virt-controller(vm 控制器)：看到 StopRequested → stopVMI → Delete VMI（设 DeletionTimestamp）
                                      （v1.2.0 vm.go:910 / v1.6.6 vm.go:1638，逻辑相同）
          → virt-controller(vmi 控制器)：VMI 删除中，但仍按 phase 分支处理（IsRunning 分支）
08:31:52  virt-handler：VMI 删除中 + domain 活着 → 优雅关机（ACPI_POWER_BTN）
08:31:59  domain ShuttingDown → 08:32:05.921 domain Shutoff（优雅关机成功，7s）
08:32:05.922 virt-handler：processVmDelete → "Signaled deletion" + DeleteDomain RPC（1ms 内！）
08:32:05.987 virt-handler：setVmPhaseForStatusReason(domain=shutoff:shutdown) → Succeeded → Update 写入 ★
          → VMI 进入 final（Succeeded）
08:32:06.394 virt-launcher 收到删除通知 → 正常退出 → pod 删除
          → virt-controller(vmi 控制器) reconcile：VMI 已 final → 走 IsFinal 分支
          → allPodsDeleted → 移除 finalizer → VMI 删除（phase=Succeeded）✓ 不重启
```

### 3.2 v1.6.6（失败：Running → Failed）

```
0s        Stop → 同上链路（virt-api → vm 控制器 stopVMI → VMI DeletionTimestamp）
09:24:50  virt-handler：优雅关机（ACPI_POWER_BTN）→ 09:24:57 domain Shutoff（成功，7s）
★ 09:24:57-09:25:43  virt-handler worker 卡 60s（无 reconcile，无 DeleteDomain）
09:25:43  virt-launcher waitForFinalNotify 45s 超时 → 主动退出 → pod 消失 ★
          → virt-controller(vmi 控制器) reconcile：VMI 还 Running + pod 不存在
          → lifecycle.go:416 无条件设 Failed → JSON Patch 写入 ★
          → VMI phase = Failed（无 phaseTransitionTimestamp）
09:25:55  virt-handler worker 恢复 → sync 入口：VMI 已是 Failed → 后续 reconcile 不再改 phase
          → vm 控制器（RerunOnFailure）：VMI Failed → 自动重启（创建新 VMI）✗
```

---

## 4. 竞态分析：谁先判 final，谁赢

Stop 流程中存在**两个独立的 phase 终态写入者**，触发条件不同：

| 写入者 | 触发条件 | 时机 | 结果 |
|--------|---------|------|------|
| virt-handler | domain shutoff:shutdown（优雅关机完成） | domain shutoff 后（需 reconcile 及时） | **Succeeded** ✓ |
| virt-launcher（间接） | waitForFinalNotify 45s 超时（没等到 DeleteDomain） | domain shutoff 后 45s | pod 消失 |
| virt-controller | VMI Running + pod 不存在 | pod 消失后 | **Failed** ✗ |

**竞态链**：
```
virt-handler 判 Succeeded（domain 驱动，~7s 内）
        vs
virt-launcher 超时退出 → pod 消失（45s）→ virt-controller 判 Failed（pod 驱动）
```

- **v1.2.0**：virt-handler 在 45s 窗口内判 Succeeded（实测 shutoff 后 1ms）→ VMI final → virt-controller 走 IsFinal 分支（不干预）→ **Succeeded**
- **v1.6.6**：virt-handler 卡 60s 错过 45s 窗口 → virt-launcher 先退出 → pod 先消失 → virt-controller 判 Failed → **Failed**

**决定性变量**：**virt-handler 是否在"domain shutoff + 45s"窗口内完成 Succeeded 判定**——这正是 v1.6.6 卡 60s 导致的差异（根因仍是 worker 卡住，待 goroutine 栈定位卡点）。

---

## 5. 关键代码位置索引

| 环节 | v1.2.0 | v1.6.6 |
|------|--------|--------|
| virt-api Stop handler（RerunOnFailure → patchVMStatusStopped） | pkg/virt-api/rest/subresource.go | pkg/virt-api/rest/subresource.go（相同逻辑） |
| vm 控制器 stopVMI（删 VMI） | pkg/virt-controller/watch/vm.go:910 | pkg/virt-controller/watch/vm/vm.go:1638 |
| vmi 控制器 updateStatus | vmi.go:522 | vmi/lifecycle.go:246 |
| vmi 控制器 IsRunning + pod 不存在 → Failed ★ | vmi.go:717-720 | lifecycle.go:416-422 |
| vmi 控制器 IsFinal → 移除 finalizer | vmi.go:697-715 | lifecycle.go:424-440 |
| virt-handler 主 reconcile | vm.go:1836 defaultExecute | vm.go:1341 sync |
| virt-handler setVmPhaseForStatusReason（domain→Succeeded） | vm.go:3241 | vm.go:2264 |
| virt-handler updateVMIStatus（Update 写入，设时间戳） | vm.go:1392 | vm.go:1045 |
| virt-handler processVmDelete（Signaled deletion + DeleteDomain） | vm.go:2375 | vm.go:1678 |
| virt-launcher waitForFinalNotify（15s KillVMI + 30s = 45s） | virt-launcher.go:279-330 | virt-launcher.go:279-330（相同） |
| vm 控制器 RerunOnFailure 重启逻辑 | vm.go:935-1010（区域） | vm/vm.go:975-990（区域） |

---

## 6. 结论

1. **watch 观察的差异（Running→Succeeded vs Running→Failed）本质是"写入者不同"**：
   - v1.2.0 的 Succeeded 由 **virt-handler** 写入（domain 优雅关机成功驱动）
   - v1.6.6 的 Failed 由 **virt-controller** 写入（VMI Running + pod 消失驱动）
2. **两个控制器都可能写 phase 终态**：virt-controller 依据 pod，virt-handler 依据 domain——**谁先触发谁赢**
3. **v1.2.0 正常**：virt-handler 先（1ms）判 Succeeded → VMI final → virt-controller 不干预
4. **v1.6.6 失败**：virt-handler 卡 60s → virt-launcher 45s 超时退出 → pod 消失 → virt-controller 先判 Failed
5. **根因不变**：virt-handler worker 卡 60s（待 goroutine 栈定位）→ 错过 45s 窗口 → virt-controller 抢写 Failed
