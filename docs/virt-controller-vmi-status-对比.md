# virt-controller VMI 状态处理对比（v1.2.0 vs v1.6.6）

> 背景：Stop 后 VMI 自动拉起问题——同样操作，v1.2.0 的 VMI 最终 **Succeeded**，v1.6.6 最终 **Failed**。
> 本文档从 virt-controller（vmi 控制器）角度做代码级对比，回答"为什么 VMI 资源本身的状态不同"。
> 结论先行：**virt-controller 侧代码无实质差异**；Failed 是 virt-controller 在"VMI Running + pod 不存在"时写入的（两版本逻辑相同），
> 差异完全在于 **virt-handler 是否在 pod 消失前把 VMI 判为 final（Succeeded）**——v1.2.0 及时（1ms），v1.6.6 卡 60s 没来得及。

---

## 1. 两版本代码结构

| 项目 | v1.2.0 | v1.6.6 |
|------|--------|--------|
| vmi 控制器主文件 | `pkg/virt-controller/watch/vmi.go`（单文件，2500+ 行） | `pkg/virt-controller/watch/vmi/lifecycle.go`（拆分目录） |
| 主 sync | `VMIController.sync`（vmi.go:1159） | `Controller.sync`（lifecycle.go:60） |
| 状态更新 | `VMIController.updateStatus`（vmi.go:522） | `Controller.updateStatus`（lifecycle.go:246） |
| VMI Patch 工具 | `prepareVMIPatch`（vmi.go 内联实现） | `prepareVMIPatch`（lifecycle.go:569，`*patch.PatchSet`） |

---

## 2. updateStatus 入口逻辑对比（完全相同）

**v1.2.0 vmi.go:522-546** 与 **v1.6.6 lifecycle.go:246-268**：

```go
vmiCopy := vmi.DeepCopy()
vmiPodExists := podExists(pod) && !isTempPod(pod)      // 两版本相同
vmiCopy, err := c.setActivePods(vmiCopy)               // 两版本相同
c.syncReadyConditionFromPod(vmiCopy, pod)              // 两版本相同
if vmiPodExists { ... c.setLauncherContainerInfo(...) } // 两版本相同
```

---

## 3. 关键分支对比（核心）

### 3.1 IsUnprocessed：DeletionTimestamp → Failed（两版本相同）

**v1.2.0 vmi.go:568-572**：
```go
case vmi.IsUnprocessed():
	if vmiPodExists {
		vmiCopy.Status.Phase = virtv1.Scheduling
	} else if vmi.DeletionTimestamp != nil || hasFailedDataVolume {
		vmiCopy.Status.Phase = virtv1.Failed
	}
```

**v1.6.6 lifecycle.go:295-299**：
```go
case vmi.IsUnprocessed():
	if vmiPodExists {
		vmiCopy.Status.Phase = virtv1.Scheduling
	} else if vmi.DeletionTimestamp != nil || hasFailedDataVolume {
		vmiCopy.Status.Phase = virtv1.Failed
	}
```

（差异仅 v1.6.6 多一个 `IsMigrationTarget` 分支，与本问题无关）

### 3.2 ★ IsRunning：pod 不存在 → Failed（两版本相同，均无 DeletionTimestamp 豁免）

**v1.2.0 vmi.go:717-720**：
```go
case vmi.IsRunning():
	if !vmiPodExists {
		vmiCopy.Status.Phase = virtv1.Failed   // ← 无条件设 Failed！
		break
	}
```

**v1.6.6 lifecycle.go:416-422**：
```go
case vmi.IsRunning():
	if !vmiPodExists {
		log.Log.Object(vmi).V(5).Infof("setting VMI to failed while running because pod does not exist")
		vmiCopy.Status.Phase = virtv1.Failed   // ← 无条件设 Failed！
		break
	}
```

**关键**：两版本**都不检查 `vmi.DeletionTimestamp`**——VMI 删除中（Stop 场景）+ pod 消失 → 一律 Failed。
v1.2.0 没走到这里，是因为 virt-handler 提前把 VMI 判成了 Succeeded（final），virt-controller 走的是 3.3 的 IsFinal 分支。

### 3.3 IsFinal：allPodsDeleted → 移除 finalizer（两版本相同）

**v1.2.0 vmi.go:697-715** 与 **v1.6.6 lifecycle.go:424-440**：

```go
case vmi.IsFinal():
	allDeleted, err := c.allPodsDeleted(vmi)
	if allDeleted {
		controller.RemoveFinalizer(vmiCopy, virtv1.VirtualMachineInstanceFinalizer)  // 两版本相同
	}
	if !c.hasOwnerVM(vmi) && len(vmiCopy.Finalizers) > 0 {
		controller.RemoveFinalizer(vmiCopy, virtv1.VirtualMachineControllerFinalizer) // 两版本相同
	}
```

---

## 4. VMI 更新机制对比（完全相同）

**v1.2.0 vmi.go:753-765** 与 **v1.6.6 lifecycle.go:513-528**：

```go
// VMI is owned by virt-handler, so patch instead of update
if vmi.IsRunning() || vmi.IsScheduled() {
	patchSet := prepareVMIPatch(vmi, vmiCopy)
	...
	_, err = c.clientset.VirtualMachineInstance(...).Patch(JSONPatchType, patchBytes, ...)  // JSON Patch
	return nil
}
// 非 Running/Scheduled（final 等）：
controller.SetVMIPhaseTransitionTimestamp(&vmi.Status, &vmiCopy.Status)
if vmiChanged { ... Update(vmiCopy, ...) }   // Update
```

**含义**：virt-controller 写入 Failed 时走 **JSON Patch**（`prepareVMIPatch` 只包含变化的字段，**不设置 PhaseTransitionTimestamp**）——
可用此特征区分 Failed 写入者（见第 7 节验证方法）。

---

## 5. 为什么 v1.2.0 是 Succeeded、v1.6.6 是 Failed（时序推演）

### v1.2.0（正常，08:31-08:32 实测日志）

```
08:32:05.921  domain shutoff（优雅关机成功）
08:32:05.922  virt-handler: Signaled deletion（shutoff 后 1ms 删 domain！）
08:32:05.987  virt-handler: VMI=Succeeded（66ms 判 final，Update 写入）
              → VMI 进入 final 状态（Succeeded）
08:32:06.394  virt-launcher 收到删除通知 → 正常退出 → pod 开始删除
              → virt-controller reconcile：VMI 已 final（Succeeded）→ 走 IsFinal 分支
              → 不移除 finalizer（pod 还在）→ 不设 Failed ✓
08:32:17      pod 全删 → allPodsDeleted → 移除 finalizer → VMI 删除（Succeeded 态）
```

**关键**：VMI 在 **pod 消失之前**已经是 final（Succeeded）→ virt-controller 永远不会走到 `IsRunning + pod 不存在 → Failed`。

### v1.6.6（失败，09:24-09:26 实测日志）

```
09:24:57      domain shutoff（优雅关机成功）
★ 09:24:57-09:25:43  virt-handler worker 卡住 60s（无任何 reconcile 日志）
              → VMI 一直是 Running，domain 一直未删
09:25:43      virt-launcher waitForFinalNotify 45s 超时 → 主动退出 → ★ pod 消失
              → virt-controller reconcile：VMI Running + pod 不存在
              → lifecycle.go:416 无条件设 Failed（JSON Patch 写入）★
09:25:55      virt-handler worker 恢复 → sync 入口看到 VMI 已是 Failed
              （"VMI is in phase: Failed | Domain does not exist"，vm.go:1364）
              → RerunOnFailure → 自动拉起 ✗
```

**关键**：VMI 在 pod 消失时仍是 **Running**（virt-handler 没来得及判 Succeeded）→ virt-controller 的 `IsRunning + !vmiPodExists` 分支命中 → Failed。

---

## 6. 结论

1. **virt-controller 侧代码无实质差异**（IsUnprocessed / IsRunning / IsFinal / 更新机制全部相同，含"无 DeletionTimestamp 豁免"的行为）
2. **Failed 的直接写入者是 virt-controller**（lifecycle.go:416 / vmi.go:717），**不是 virt-handler 的 calculateVmPhaseForStatusReason**
3. **两版本唯一的行为差异在 virt-handler 的及时性**：
   - v1.2.0：shutoff 后 1ms 删 domain、66ms 判 Succeeded → VMI 先于 pod 消失进入 final → virt-controller 不干预
   - v1.6.6：worker 卡 60s → virt-launcher 45s 超时退出 → pod 先消失 → virt-controller 判 Failed
4. **根因链**：virt-handler worker 卡 60s（卡点待 goroutine 栈定位）→ virt-launcher 超时退出 → pod 消失 → virt-controller 写入 Failed → RerunOnFailure 重启

---

## 7. 验证方法（区分 Failed 写入者）

```bash
kubectl get vmi <vmi名> -o jsonpath='{.status.phaseTransitionTimestamp}{"\n"}{.status.phase}{"\n"}'
```

- **无 phaseTransitionTimestamp** → virt-controller 写入（JSON Patch 不设时间戳）→ 验证本结论
- 有 phaseTransitionTimestamp → virt-handler 写入（SetVMIPhaseTransitionTimestamp）

```bash
# virt-controller 侧证据（V(5) 日志，需调高日志级别）
kubectl logs -n kubevirt -l kubevirt.io=virt-controller --since=10m | grep -i "setting VMI to failed\|patching VMI"
```

---

## 8. 修复方向（更新）

| 方向 | 内容 | 备注 |
|------|------|------|
| **B（优先）** | virt-launcher `waitForFinalNotify` 在 domain 已 `shutoff + reason=shutdown` 时不超时退出 → pod 存活 → virt-controller 不设 Failed → virt-handler 恢复后判 Succeeded | 直接阻断"pod 消失 → Failed"链路；注意 pod terminationGracePeriod 限制 |
| A2（已提交 50a61a3d21） | virt-handler handleVMIShutdown 保持 requeue | 单独无效（worker 卡时 requeue 也排队），作为兜底 |
| C | virt-controller 对"VMI 删除中（DeletionTimestamp 存在）+ pod 消失"豁免 Failed | 语义合理但属社区行为变更，需谨慎/提 issue |
| 根因 | 定位 virt-handler worker 卡 60s 的卡点（goroutine 栈 / logVerbosity=4） | 治本 |

---

## 9. 补充发现：v1.6.6 缺失 "Signaled deletion" 日志的证据链

**现象**：v1.2.0 日志有 `Signaled deletion for 27f237e6...`（08:32:05.922，vm.go:2383），v1.6.6 日志（09:25:55 附近）**没有**。

**原因**：该日志只在 `GetVerifiedLauncherClient` 成功（virt-launcher 连接存活）时打印：

```go
// v1.6.6 vm.go:1678 / v1.2.0 vm.go:2375（两版本完全相同）
func processVmDelete(vmi) error {
	client, err := c.launcherClients.GetVerifiedLauncherClient(vmi)
	if err == nil {                                    // ★ 连接活着才进来
		log.Log.Object(vmi).Infof("Signaled deletion for %s", ...)  // ← 这条日志
		c.recorder.Event(...)
		err = client.DeleteDomain(vmi)                 // ★ DeleteDomain RPC（virt-launcher 删除通知的来源）
		...
	}
	return nil
}
```

**v1.6.6 缺失的原因**：
```
09:25:43  virt-launcher 已超时退出（45s 等不到删除通知）
09:25:55  processVmDelete 才执行 → GetVerifiedLauncherClient 返回 err（连接已断）
          → if err == nil 不成立 → 不打日志 → 不发 DeleteDomain → 直接 return nil
```

**深层意义（关键闭环证据）**：`DeleteDomain` RPC 正是 virt-launcher `waitForFinalNotify`（virt-launcher.go:279-330）等待的删除通知：
```
v1.2.0:  virt-handler 及时发 DeleteDomain（shutoff 后 1ms）→ virt-launcher 收到通知
         → 08:32:06.394 "Waiting on final notifications" → 正常退出 ✓
v1.6.6:  ★ virt-handler 卡 60s 没发 DeleteDomain → virt-launcher 等 45s 超时 → 自己退出（09:25:43）
         → 退出后 processVmDelete 才执行 → 连接已断 → Signaled deletion 缺失（DeleteDomain 从未发出）✗
```

**"Signaled deletion 缺失" = "virt-handler 删 domain 太晚（virt-launcher 已退）"的直接日志证据**，与第 5 节时序推演完全闭环：

| 证据（v1.6.6 日志） | 含义 |
|------|------|
| 65s 无 "VMI is in phase" 日志 | virt-handler 该 key 未被 worker 处理（卡 60s） |
| virt-launcher 09:25:43 退出 | waitForFinalNotify 45s 超时（没等到 DeleteDomain） |
| **缺 "Signaled deletion"** | **DeleteDomain 从未发出（processVmDelete 执行时连接已断）** |
| virt-controller 写 Failed（lifecycle.go:416） | VMI Running + pod 消失 |
| virt-handler 入口见 "VMI is in phase: Failed" | 恢复太晚，VMI 已被 virt-controller 置 Failed |

---

## 附：相关代码位置索引

| 内容 | v1.2.0 | v1.6.6 |
|------|--------|--------|
| vmi 控制器 updateStatus | pkg/virt-controller/watch/vmi.go:522 | pkg/virt-controller/watch/vmi/lifecycle.go:246 |
| IsUnprocessed（DeletionTimestamp→Failed） | vmi.go:568-572 | lifecycle.go:295-299 |
| IsRunning（pod 不存在→Failed）★ | vmi.go:717-720 | lifecycle.go:416-422 |
| IsFinal（移除 finalizer） | vmi.go:697-715 | lifecycle.go:424-440 |
| Running/Scheduled → JSON Patch | vmi.go:753-765 | lifecycle.go:513-528 |
| 其他 → Update + PhaseTransitionTimestamp | vmi.go:767-780 | lifecycle.go:535-550 |
| virt-handler sync 入口日志（"VMI is in phase"） | vm.go:1856-1863 | vm.go:1362-1369 |
| virt-handler worker 卡住窗口（实测） | 无（1ms 处理） | 09:24:57-09:25:55（60s） |
