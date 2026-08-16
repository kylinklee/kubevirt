# KubeVirt 1.6.6 → 1.2.0 降级适配分析（2026-08-17 二次修正版）

> 本文档是 KubeVirt 降级（v1.6.6 → v1.2.0）的适配分析快照，结论基于 `v1.2.0`（commit `b2af7b619c`）与 `v1.6.6`（commit `51395ba146`）两个 tag 的源码逐行对比。
>
> 前置背景：所有需要降级的设备都先是从 1.2.0 升级上来的；客户端始终使用 1.2.0 的 client-go，不主动创建任何高版本资源；不启用任何 1.6.6 新特性；CR 全程未变更，`workloadUpdateMethods` 确认为空 `[]`。

---

## 0. TL;DR

| # | 问题 | 结论 | 动作 |
|---|------|------|------|
| 1 | 1.6.6 是否新增了 CRD kind | **没有**。1.2.0 与 1.6.6 的 CRD 工厂列表完全相同（16 个）。 | 无需备份/删除任何"新增 CRD" |
| 2 | 那真正变化的 CRD 是什么 | **已存在 CRD 的 storage 版本变化**：5 个 CRD 从 `v1alpha1` 切到 `v1beta1`（snapshot 系 3 个 + export + clone）。 | 见 §2 |
| 3 | snapshot/export/clone 的 CR 实例会被自动创建吗 | **不会**。CRD 由 operator 自动创建；CR 实例只由用户/client 创建。生产无 CR 数据 → 可直接删 CRD 重建。 | 见 §3 |
| 4 | 降级时 virt-launcher 为何自动重启（升级不重启） | **1.2.0 handler 把「Running VMI + domain 瞬态缺失」硬编码判 Failed**，VM controller（runStrategy:Always）随即重启。**与 workloadUpdateMethods 无关，也与「handler 管不了 1.6.6 launcher」无关**（协议/序列化/ghost record 全兼容，旧结论已撤回）。 | 见 §4 |
| 5 | 1.6.6 独有、1.2.0 不管理的孤儿资源 | VAP + Binding（node-restriction）、virt-synchronization-controller Deployment。 | 降级后手动删，见 §5 |

---

## 1. 核心结论：1.2.0 → 1.6.6 之间没有新增 CRD kind

`pkg/virt-operator/resource/generate/install/strategy.go` 的 `GenerateCurrentInstallStrategy` 里，CRD 工厂函数列表在两个版本中**完全一致**，均为 16 个：

```
vmi, vmipreset, vmirs, vm, vmim,
vmsnapshot, vmsnapshotcontent, vmrestore,
vminstancetype, vmclusterinstancetype, vmpool,
migrationpolicy, vmpreference, vmclusterpreference,
vmexport, vmclone
```

（`kubevirts` 这个 CRD 由 operator 的静态清单安装，不进 apply 循环，两个版本也一致。）

所以"高版本新增的 CRD 低版本不存在"这个命题**不成立**。真正需要处理的是**已存在 CRD 的 served/storage 版本变化**（下一节），以及 1.6.6 新增的**非 CRD** 资源（§5）。

---

## 2. 真正要处理的 CRD 问题：5 个 CRD 的 storage 版本 v1alpha1 → v1beta1

`pkg/virt-operator/resource/generate/components/crds.go` 对比结果：

| CRD | 1.2.0 | 1.6.6 |
|-----|-------|-------|
| virtualmachinesnapshots | 仅 v1alpha1（storage） | v1alpha1（served，非 storage）+ **v1beta1（storage）** |
| virtualmachinesnapshotcontents | 同上 | 同上 |
| virtualmachinerestores | 同上 | 同上 |
| virtualmachineexports | 同上 | 同上 |
| virtualmachineclones | 同上 | 同上 |

（instancetype / preference 系 CRD 在 1.6.6 把 v1alpha1/v1alpha2 标记为 `Served:false`，降级时重新 serve，属于"加 served 版本"，是允许的，无害。）

**降级为什么会有坑**：1.2.0 operator 的 `createOrUpdateCrd` 会把 CRD spec 用 `WithReplace("/spec")` 整体覆盖成"只 serve v1alpha1"。如果 apiserver 里这 5 个 CRD 的 storage 版本还是 v1beta1，且**存在 v1beta1 的 CR 实例**，apiserver 会拒绝这次 patch（不允许移除当前 storage 版本）。而一旦这些 CRD 处于半新半旧状态，1.2.0 组件的 informer（用的是 v1alpha1 REST client）反而能正常工作——这正是本分支上"跳版本升级 informer 条件创建"修复的反向问题，方向相反，风险更低。

**处理方式（两种）**：

1. **推荐（生产无 CR 数据时）**：直接删掉这 5 个 CRD，让 1.2.0 operator 重建为 v1alpha1-only。因为从不创建 snapshot/export/clone 的 CR，删除不会有任何数据损失。删 CRD 的代价是"连带删除该类型的全部 CR"，而生产 CR 数量为 0，所以零损失。
2. **保守**：按升级的反向步骤——先把 storage 版本切回 v1alpha1（保留 v1beta1 作为 served），等 `status.storedVersions` 收敛到只剩 v1alpha1，再 drop 掉 v1beta1，最后让 1.2.0 operator 接管。步骤多、容易在 apiserver 校验上出错，除非确实有 CR 数据，否则不建议。

---

## 3. 关于"这 5 个资源有组件会自动创建吗"

- **CRD 本体**：会，由 virt-operator 自动创建（两个版本的 `createOrUpdateCrds` 都创建全部 16 个 CRD）。所以降级后 CRD 必然存在，不需要手动补。
- **CR 实例**（VirtualMachineSnapshot / VirtualMachineSnapshotContent / VirtualMachineRestore / VirtualMachineExport / VirtualMachineClone）：**不会被任何组件自动创建**。它们只由以下途径产生：
  - 用户用 virtctl 或 client-go 主动创建；
  - 上层平台（如管理端）主动创建。

生产明确"不会主动创建"且客户端是 1.2.0 client-go，因此集群里这 5 类 CR 的实例数必然为 0。**结论：不需要备份 CR；直接删 CRD 重建即可，无数据损失。**

---

## 4. 降级时 virt-launcher 自动重启的根因（重点，已排除 workloadUpdateMethods）

### 4.1 先排除 workload-updater

已确认 `workloadUpdateMethods: []` 为空。`pkg/virt-controller/watch/workload-updater/workload-updater.go` 的 `getUpdateData()`：

```go
automatedMigrationAllowed := false
automatedShutdownAllowed := false
for _, method := range kv.Spec.WorkloadUpdateStrategy.WorkloadUpdateMethods {
    if method == virtv1.WorkloadUpdateMethodLiveMigrate {
        automatedMigrationAllowed = true
    } else if method == virtv1.WorkloadUpdateMethodEvict {
        automatedShutdownAllowed = true
    }
}
```

空列表 → 两个开关都是 false → 即便 `isOutdated()` 判定 launcher 镜像过期（1.6.6 vs 1.2.0），也**不会**发起迁移或驱逐。`types.go` 注释也明确"空列表 = 不做自动 workload 更新"。所以 **workload-updater 不是根因**。

### 4.2 先承认你的前提，撤回我上一版的解释

你的挑战是对的。上一版我写"1.2.0 handler 无法管理 1.6.6 launcher"，但它与你陈述的事实自相矛盾：**升级时 launcher 从不重启 → 它是 1.2.0 handler 拉起的 → 降级后回到 1.2.0 handler，理应还能管它**。我把代码逐行重读了一遍，结论是：**跨版本兼容性根本不是问题**。上一版引用的 StarlingX 那句"older virt-handler cannot manage VMs started by a newer version"在这个场景里**用错了**——它针对的是"新版本 handler 启动的 VM"，而你们的 VM 恰恰不是。

逐项验证（全部兼容，推翻"管不了"）：

| 维度 | 结论 |
|------|------|
| cmd gRPC 协议 | `cmd.proto` 在 1.6.6 只**新增** `ResetVirtualMachine`、`GetDomainDirtyRateStats` 两个 RPC；`GetDomain`/`SyncVirtualMachine` 等基础 RPC 完全不变。`CmdVersion = 1`、`SupportedCmdVersions = [1]` 两版本一致，版本协商对旧 client 调旧方法零影响。 |
| socket 路径 | `launcher-sock` 文件名、`/pods/<uid>/volumes/kubernetes.io~empty-dir/sockets/` 路径两版本一致；`FindSocketOnHost` 都按 `vmi.Status.ActivePods` 找。 |
| domain 序列化 | handler `GetDomain()` 用 `json.Unmarshal` **非 strict**；1.6.6 domain 结构多出的字段（schema.go 350 vs 330 个字段，如 `vmport`、`slice` 等）被 1.2.0 的 `api.Domain` 直接忽略，不影响 `domain.Status.Status` 等关键字段。 |
| ghost record 磁盘格式 | 两版本都写到 `<VirtPrivateDir>/ghost-records/<uid>`，JSON 字段 `{name, namespace, socketFile, uid}` 完全相同，互相可读。 |
| **watchdog 文件（关键反证）** | 上一版说"1.6.6 删了 watchdog → 1.2.0 handler 回退失效"。**这是错的**：`pkg/watchdog.WatchdogFileUpdate()` 在 v1.2.0 里**没有任何调用者**（死代码），watchdog 文件从来就没有组件在写。1.2.0 handler 的失联判定 `isLauncherClientUnresponsive` 对 `launcher-sock` 恒走 socket 监控分支（`SocketMonitoringEnabled == true`），watchdog 回退永不触发。 |

### 4.3 真正的代码链：domain 瞬态缺失 → 硬编码判 Failed

这是两版本**完全一致**的、无保护的一段（v1.2.0 `pkg/virt-handler/vm.go` `calculateVmPhaseForStatusReason`）：

```go
if domain == nil {
    switch {
    case vmi.IsScheduled(): ...          // 仅 Scheduled 阶段才查 socket 是否 unresponsive
    case !vmi.IsRunning() && !vmi.IsFinal():
        return v1.Scheduled, nil
    case !vmi.IsFinal():
        // That is unexpected. We should not be able to delete a VirtualMachineInstance before we stop it.
        // However, if someone directly interacts with libvirt it is possible
        return v1.Failed, nil            // ← Running 且 domain==nil → 无条件 Failed，不查 socket
    }
}
```

注意区别：**只有 `Scheduled` 阶段**会去 `isLauncherClientUnresponsive`（查 socket 存活、等 pod 初始化）；一旦 VMI 已是 `Running`，只要 domain informer 里查不到 domain，就**直接判 Failed**，既不等、也不查 launcher 到底死没死。随后 `defaultExecute → updateVMIStatus → setVmPhaseForStatusReason` 把 `Failed` 写回 VMI；VM controller `startStop`（RunStrategyAlways）`if forceRestart || vmi.IsFinal()` → `stopVMI` 删 VMI → 下一轮重建 → virt-launcher pod 重拉。**这就是"降级时 workload 自动重启"**——链路的触发条件只要求「Running VMI + handler 侧 domain 认知短暂为空」，与 launcher 是 1.2.0 还是 1.6.6 无关。

### 4.4 那"domain 认知短暂为空"从哪来（升级为何不触发、降级才触发）

这段判定逻辑两版本相同，所以不对称性一定来自 **handler 重启后 domain informer 的冷启动行为差异**。已确认的关键差异：

1. **启动同步门**（`cmd/virt-handler/virt-handler.go`）：
   - v1.2.0：`cache.WaitForCacheSync(stop, vmiSourceInformer.HasSynced, factory.CRD().HasSynced, factory.KubeVirt().HasSynced)` —— **main 层不等 domain informer**（controller `Run()` 里才等，但 `Run` 是在 main 的 `WaitForCacheSync` 之后才 `go` 起来）。
   - v1.6.6：`WaitForCacheSync(stop, ..., domainSharedInformer.HasSynced, ...)` —— **main 层显式等 domain informer 完成首轮同步**。

2. **domain informer 首轮 List 的来源**（`pkg/virt-handler/cache/`）：
   - v1.2.0：`List()` 走 `listAllKnownDomains()`，靠**现场扫描 `/pods`**（`ListAllSockets` 逐 pod 找 `launcher-sock`）再逐个 `client.GetDomain()` 拉 domain。冷启动时依赖 `/pods` 挂载 + 逐 socket gRPC 往返，任何一个环节慢/失败都会让某个 VMI 的 domain 在首轮缺失。
   - v1.6.6：`List()` 走 **ghost record 全局表**（`GhostRecordGlobalStore`，handler 内存里持久化的、上次从 socket 建立的记录），`listAllKnownDomains` 也从 ghost record 拿 socket 列表。

   即 1.2.0 的 domain 冷启动更"脆"：要现场重新枚举 /pods 并逐 socket GetDomain；1.6.6 先有 ghost record 兜底、再在 main 层等 HasSynced。这能解释**为什么同一台机器、同样的 Running VMI，升级到 1.6.6 时 handler 重启不判死、降级回 1.2.0 时 handler 重启会判死**。

> ⚠️ 诚实声明：§4.4 的"冷启动竞态"是**唯一能同时解释升级/降级不对称**的静态证据链，但"某台具体 VM 的 domain 短暂 nil"的确切触发瞬间（首轮 List 慢、某 socket GetDomain 超时、还是 stale socket 误删 domain）需要运行时日志最终钉死。判据很明确：降级窗口内 virt-handler 日志会出现 `VMI is in phase: Running | Domain does not exist` 紧接一条 `VMI ... Failed` 事件，且**没有任何 launcher 崩溃日志**。若你们有这批日志，我可以做最终确认。

### 4.5 这意味着什么（修复方向）

1. **真修在 1.2.0 handler**（不是 1.6.6）：把 `calculateVmPhaseForStatusReason` 里 `case !vmi.IsFinal()` 的 `return v1.Failed` 改成先 `isLauncherClientUnresponsive(vmi)`——launcher 确实失联才 Failed，否则 `Queue.AddAfter(1s)` 等 domain 事件。这样即便 domain 冷启动短暂缺失，也不会误判死、触发 VM controller 硬重启。**这与 launcher 是哪个版本无关，纯 handler 健壮性问题**（上游在 1.6.6 用"main 层等 domain HasSynced"间接规避了，但没有改判定本身）。
2. 若 1.2.0 是你们维护的定版分支、可以打补丁，上面的改动就是"通过改源码实现降级兼容"的最小正确解法。**注意：在你的场景里（升级时 launcher 没重拉），launcher 本来就是 1.2.0 镜像，降级后 handler 和 launcher 天然同版本，打完补丁就真的可以做到零重启、零迁移地降级**；若 1.2.0 冻结不可改，则只能流程规避（§6 方案 A）。
3. **改 1.6.6 源码帮不上这个场景**：降级后实际运行的是 1.2.0 handler，1.6.6 侧的兼容 shim（比如让 1.6.6 launcher 重写 legacy watchdog 文件）无法阻止 1.2.0 handler 在 `domain==nil` 时硬判 Failed——何况 watchdog 文件在 1.2.0 本来就是死代码、没人读它。真正该改的、且只需要改的就是 1.2.0 handler 那一个分支。

### 4.6 为什么官方/StarlingX 说"downgrade 要停 VM"

KubeVirt 官方不承诺支持"VM 运行中降级"，本质是因为 handler 侧的 phase 判定对 domain 认知缺失**零容忍**（`Running && domain==nil → Failed`），任何 handler 冷启动/重连窗口都可能触发。官方把这个限制文档化，而不是去修 handler 的健壮性。所以：**官方语义下，降级前停 VM 是唯一"受支持"的做法；你们若要"不停 VM 降级"，就必须给 1.2.0 handler 打 §4.5 的补丁。**

---

## 5. 1.6.6 独有、1.2.0 不会管理的孤儿资源（降级后需手动删）

| 资源 | 说明 | 1.2.0 行为 | 风险 |
|------|------|------------|------|
| ValidatingAdmissionPolicy `kubevirt-node-restriction-policy` + ValidatingAdmissionPolicyBinding `kubevirt-node-restriction-binding` | 1.6.6 新增（`components/validatingadmissionpolicy.go`，FailurePolicy: Fail / Deny），限制对 node 的操作 | 1.2.0 无此组件，`delete.go` 只删自己 strategy 里的对象 → **残留** | 可能拦截 1.2.0 virt-handler 对 node 的写入 |
| Deployment `virt-synchronization-controller` | 1.6.6 新增（`strategy.go` 里 `synchronizationControllerDeployment`） | 1.2.0 无 → **残留** | 一直 CrashLoop/无意义占用 |

这两类在降级完成后要手动删除（VAP 是 K8s 1.30+ 的 beta API，注意 apiserver 版本）。

---

## 6. 推荐降级流程

> 根因已修正（§4）：重启的触发是「Running VMI + 1.2.0 handler 冷启动时 domain 认知短暂为空 → 硬判 Failed」，与 launcher 版本无关。据此，流程有两档选择。

**方案 A（零改动，官方支持路径）**：

1. **先停/冻结全部运行 VM**：`virtctl stop`，或迁到别的节点（若集群支持）。这是官方唯一"受支持"的降级姿势。
2. **处理 5 个 storage 版本 CRD**：确认无 snapshot/export/clone CR 实例后，直接 `kubectl delete crd`（virtualmachinesnapshots / virtualmachinesnapshotcontents / virtualmachinerestores / virtualmachineexports / virtualmachineclones）。1.2.0 operator 随后自动重建为 v1alpha1-only。
3. **apply 低版本 operator/CR**（imageTag 指向 1.2.0）。管理面组件滚降级（符合预期）。
4. **删除孤儿资源**：`kubectl delete validatingadmissionpolicy kubevirt-node-restriction-policy`、`kubectl delete validatingadmissionpolicybinding kubevirt-node-restriction-binding`、`kubectl delete deployment virt-synchronization-controller -n kubevirt`。
5. **校验**：operator `Deployed`、组件版本、VMI 状态。
6. **分批重启 VM** 到 1.2.0 launcher（维护窗口）。

**方案 B（不停 VM 降级，需改源码）**：

在 1.2.0 handler 打 §4.5 的补丁——`calculateVmPhaseForStatusReason` 的 `case !vmi.IsFinal()` 分支先 `isLauncherClientUnresponsive(vmi)`，launcher 确死才 Failed，否则 requeue 1s 等 domain 事件。这样 Running VMI 在 handler 冷启动窗口内不会被误判死。之后按方案 A 的步骤 2–5 执行即可。**在你的场景下（升级时 launcher 从未重拉 → launcher 是 1.2.0 镜像），补丁后降级全程无需停 VM、无需重启 launcher，天然零扰动。**

---

## 7. 附：关键源码位置

| 文件（按 tag） | 内容 |
|----------------|------|
| `pkg/virt-operator/resource/generate/install/strategy.go` | CRD 工厂列表（两版本 16 个一致）；1.6.6 额外 wire VAP + sync-controller |
| `pkg/virt-operator/resource/generate/components/crds.go` | 5 个 CRD 的 storage 版本 v1alpha1→v1beta1 |
| `pkg/virt-operator/resource/apply/crds.go` | `createOrUpdateCrd` 用 `WithReplace("/spec")` 覆盖 spec（降级冲突点） |
| `pkg/virt-controller/watch/workload-updater/workload-updater.go` | `getUpdateData`：空 methods → 不迁移不驱逐 |
| `pkg/virt-handler/vm.go`（1.2.0） | `calculateVmPhaseForStatusReason`：domain==nil 且非 Final → **无条件 Failed**（不查 socket，只 Scheduled 阶段才查）；`defaultExecute`/`updateVMIStatus` 写回 Failed |
| `pkg/virt-controller/watch/vm.go`（1.2.0） | `startStop` RunStrategyAlways：`vmi.IsFinal()` → stopVMI 重启 |
| `cmd/virt-handler/virt-handler.go` | **不对称根源**：1.2.0 main 层 `WaitForCacheSync` 不等 domainInformer；1.6.6 显式等 `domainSharedInformer.HasSynced` |
| `pkg/virt-handler/cache/cache.go`（1.2.0） | domain informer 冷启动靠扫 `/pods` 逐个 GetDomain（脆）；`WatchdogFileUpdate` 是死代码（watchdog 文件无人写） |
| `pkg/virt-handler/cache/domain-watcher.go`（1.6.6） | domain informer 冷启动靠 ghost record 全局表兜底 |
| `pkg/handler-launcher-com/cmd/v1/` + `notify/v1/` | 协议层完全兼容（CmdVersion=1、notify.proto 相同、cmd.proto 仅多 2 个 RPC、domain 序列化非 strict） |

---

*参考：StarlingX 官方文档 [Stop VMs Before Platform Rollback](https://docs.starlingx.io/kube-virt/kubevirt-stop-vms-before-platform-rollback.html)（"KubeVirt does not support downgrades while VMs are running"——其前提是"新版本 handler 启动的 VM"，不适用于本场景中"1.2.0 handler 启动的 VM"）、社区 [kubevirt/kubevirt#15018](https://github.com/kubevirt/kubevirt/issues/15018)。*
