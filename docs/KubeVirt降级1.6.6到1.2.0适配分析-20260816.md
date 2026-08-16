# KubeVirt 1.6.6 → 1.2.0 降级适配分析（2026-08-16 代码级最终版）

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
| 4 | 降级时 virt-launcher 为何自动重启（升级不重启） | **1.2.0 virt-handler 无法管理 1.6.6 拉起的 launcher**，VMI 被判 Failed，VM controller（runStrategy:Always）重启到 1.2.0 镜像。**与 workloadUpdateMethods 无关**。 | 见 §4 |
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

### 4.2 真正的机制：1.2.0 virt-handler 无法管理 1.6.6 launcher

KubeVirt 官方（StarlingX 发行文档）明确写道：

> "KubeVirt does not support downgrades while VMs are running. The older virt-handler binary cannot manage VMs that were started by a newer version. This results in virt-handler entering a requeue loop..."

这与观察到的现象完全吻合。代码链如下（v1.2.0）：

**第 1 步：handler 侧把 VMI 判死。** `pkg/virt-handler/vm.go` 的 `calculateVmPhaseForStatusReason`：

```go
if domain == nil {
    switch {
    case vmi.IsScheduled(): ...
    case !vmi.IsRunning() && !vmi.IsFinal():
        return v1.Scheduled, nil
    case !vmi.IsFinal():
        // That is unexpected. ... if someone directly interacts with libvirt it is possible
        return v1.Failed, nil
    }
}
```

1.2.0 handler 重启后，它的 domain informer（`cache.go` 的 `newListWatchFromNotify` + socket 监控 + `handleStaleSocketConnections`）对 1.6.6 老 launcher 的 domain 感知可能为空；即使能连上 socket 发 `SyncVirtualMachine`，`SyncVMI` 里对 1.6.6 生成的新 domain 结构同步也可能报错（`processVmUpdate` / `handleSyncError`），于是反复 requeue、置 Synchronized=False。一旦走到 `domain == nil && vmi 非 Final` 分支，就返回 `Failed`。

**第 2 步：VM controller 重启 VM。** `pkg/virt-controller/watch/vm.go` 的 `startStop()`，`RunStrategyAlways` 分支：

```go
if forceRestart || vmi.IsFinal() {
    // ... stopping VMI and letting it start in next step
    vm, err = c.stopVMI(vm, vmi)
    ...
}
```

VMI 进入 Failed → `stopVMI` 删 VMI → 下一轮 reconcile 重建 VMI → virt-launcher pod 按 CR 里已改成 1.2.0 的 `imageTag` 重新拉镜像启动。**这就是"降级时 workload 自动重启"。**

### 4.3 为什么升级不重启、降级才重启（不对称）

关键在 `pkg/watchdog`：这个包在 **1.2.0 存在，1.6.6 已整体删除**。

- **升级方向**（1.2.0 → 1.6.6）：1.6.6 的 virt-handler 显式保留了向后兼容路径——legacy socket 目录（`/var/run/kubevirt/sockets`）、legacy watchdog 文件回退、`migrationTransport` 探测等。旧 launcher 继续被新 handler 管理，VM 不重启。
- **降级方向**（1.6.6 → 1.2.0）：1.2.0 的 virt-handler 不包含对"1.6.6 新 launcher"的兼容代码。1.6.6 launcher 不再写 watchdog 文件（该机制在 1.6.6 已删），而 1.2.0 handler 的 `isLauncherClientUnresponsive` 在 socket 监控失败时会回退到 watchdog 文件判断——对 1.6.6 launcher 这个回退直接失效。管理能力断链 → VMI Failed → 重启。

补充确认（排除协议层问题）：cmd gRPC 协议在两个版本**基本一致**——`CmdVersion = 1`、`SupportedCmdVersions = [1]`、`cmd/info/info.proto` 完全相同、`notify.proto` 完全相同；`cmd.proto` 只在 1.6.6 多了 `ResetVirtualMachine` 和 `GetDomainDirtyRateStats` 两个 RPC（新增 RPC 不影响旧 client 调用旧方法）。socket 路径（`/pods/<uid>/volumes/kubernetes.io~empty-dir/sockets/launcher-sock`）也一致。**所以不是协议/socket 不兼容，而是 handler 对"新 launcher"的管理能力不兼容。**

### 4.4 这意味着什么

降级时**没有任何办法让运行中的 1.6.6 virt-launcher 原地平滑降到 1.2.0**。`workloadUpdateMethods` 改不改都一样。唯一正确的做法是：

1. 降级 operator/CR 前，先处理运行中的 VM——要么 `virtctl stop` 停掉，要么先迁到别的节点（如果集群支持，且目标仍是 1.6.6 launcher）；
2. 对 `runStrategy: Always` 的 VM，即使不主动停，VM controller 也会在 VMI 进入 Failed 后自动重启到 1.2.0 launcher（官方文档明确写了这一点）——**这在生产意味着一次不受控的 VM 重启，务必在维护窗口做，或提前主动停**；
3. 对 `runStrategy: Manual / RerunOnFailure` 的 VM，降级后需手动 `virtctl start`。

---

## 5. 1.6.6 独有、1.2.0 不会管理的孤儿资源（降级后需手动删）

| 资源 | 说明 | 1.2.0 行为 | 风险 |
|------|------|------------|------|
| ValidatingAdmissionPolicy `kubevirt-node-restriction-policy` + ValidatingAdmissionPolicyBinding `kubevirt-node-restriction-binding` | 1.6.6 新增（`components/validatingadmissionpolicy.go`，FailurePolicy: Fail / Deny），限制对 node 的操作 | 1.2.0 无此组件，`delete.go` 只删自己 strategy 里的对象 → **残留** | 可能拦截 1.2.0 virt-handler 对 node 的写入 |
| Deployment `virt-synchronization-controller` | 1.6.6 新增（`strategy.go` 里 `synchronizationControllerDeployment`） | 1.2.0 无 → **残留** | 一直 CrashLoop/无意义占用 |

这两类在降级完成后要手动删除（VAP 是 K8s 1.30+ 的 beta API，注意 apiserver 版本）。

---

## 6. 推荐降级流程

1. **冻结并处理 workload**（最重要，先做）：确认全部运行 VM 的处置策略。生产要求零扰动 → 提前 `virtctl stop`，或迁移；接受重启 → 只对 runStrategy:Always 的 VM 依赖自动重启（会有短暂中断）。
2. **处理 5 个 storage 版本 CRD**：确认无 snapshot/export/clone CR 实例后，直接 `kubectl delete crd` 这 5 个 CRD（virtualmachinesnapshots / virtualmachinesnapshotcontents / virtualmachinerestores / virtualmachineexports / virtualmachineclones）。1.2.0 operator 随后会自动重建为 v1alpha1-only。
3. **apply 低版本 operator/CR**（imageTag 指向 1.2.0）。管理面组件滚降级（符合预期）。
4. **删除孤儿资源**：`kubectl delete validatingadmissionpolicy kubevirt-node-restriction-policy`、`kubectl delete validatingadmissionpolicybinding kubevirt-node-restriction-binding`、`kubectl delete deployment virt-synchronization-controller -n kubevirt`。
5. **校验**：operator `Deployed`、组件版本、VMI 状态。
6. **分批重启 VM**到 1.2.0 launcher（维护窗口）。

---

## 7. 附：关键源码位置

| 文件（按 tag） | 内容 |
|----------------|------|
| `pkg/virt-operator/resource/generate/install/strategy.go` | CRD 工厂列表（两版本 16 个一致）；1.6.6 额外 wire VAP + sync-controller |
| `pkg/virt-operator/resource/generate/components/crds.go` | 5 个 CRD 的 storage 版本 v1alpha1→v1beta1 |
| `pkg/virt-operator/resource/apply/crds.go` | `createOrUpdateCrd` 用 `WithReplace("/spec")` 覆盖 spec（降级冲突点） |
| `pkg/virt-controller/watch/workload-updater/workload-updater.go` | `getUpdateData`：空 methods → 不迁移不驱逐 |
| `pkg/virt-handler/vm.go`（1.2.0） | `calculateVmPhaseForStatusReason`：domain==nil 且非 Final → Failed |
| `pkg/virt-controller/watch/vm.go`（1.2.0） | `startStop` RunStrategyAlways：`vmi.IsFinal()` → stopVMI 重启 |
| `pkg/watchdog` | 1.2.0 存在、1.6.6 已删除（降级不对称的根源） |
| `pkg/virt-handler/cache/cache.go`（1.2.0） | socket 监控 + watchdog 回退（对 1.6.6 launcher 失效） |
| `pkg/handler-launcher-com/cmd/v1/` + `notify/v1/` | 协议层兼容（CmdVersion=1、notify.proto 相同、cmd.proto 仅多 2 个 RPC） |

---

*参考：StarlingX 官方文档 [Stop VMs Before Platform Rollback](https://docs.starlingx.io/kube-virt/kubevirt-stop-vms-before-platform-rollback.html)（"KubeVirt does not support downgrades while VMs are running"）、社区 [kubevirt/kubevirt#15018](https://github.com/kubevirt/kubevirt/issues/15018)。*
