# virt-operator 升级不生效定位：v1.2.0 → v1.6.6 组件未跟随升级

> 定位日期：2026-08-14
> 分支：upgrade-v1.2-to-v1.6.6（基于 v1.6.6 + 自研镜像定制）
> 状态：**结论已锁定方向，待生产环境日志/CRD 版本确认后给出代码修复**

---

## 0. 问题现象

生产环境从 v1.2.0 升级到 v1.6.6，操作分两步：

1. 将 `kubevirt-operator.yaml` 中镜像改为 v1.6.6 并 apply → virt-operator 滚动升级完成
2. `kubectl patch kv kubevirt -n kubevirt --type=json -p '[{ "op": "add", "path": "/spec/imageTag", "value": "v1.6.6" }]'` → **没有任何组件自动升级**

补充确认的事实：

- 两个 virt-operator Pod 均已是 v1.6.6 镜像
- 发现不升级后执行 `kubectl delete po -n kubevirt --all` 重拉全部组件 → **除 operator 外组件仍是 v1.2.0-h3**
- fa04d0f8 已修复 snapshot/export/clone 的 v1beta1 informer 兼容问题（与本问题同类但不同范围，见 §5）

KV CR 的关键状态（诊断核心证据）：

| 字段 | 值 | 说明 |
|---|---|---|
| `spec.imageTag` | `v1.6.6` | patch 已生效 |
| `metadata.generation` | `4` | patch 后的 generation |
| `status.observedGeneration` | `3` | **新 operator 从未 reconcile 到 gen 4** |
| `status.targetKubeVirtVersion` | `v1.2.0-h3` | 目标版本未变 |
| `status.observedKubeVirtVersion` | `v1.2.0-h3` | 已安装版本未变 |
| `status.targetDeploymentID` == `observedDeploymentID` | `14a82789...` | operator 认为"目标 == 现状"，无升级动作 |
| `status.targetDeploymentConfig.virtOperatorImage` | `simbaos.io/virt-operator:v1.2.0-h3` | 目标 operator 镜像仍是旧值 |
| `status.operatorVersion` | `v0.0.0-master+$Format:%h$` | 镜像构建未注入版本 ldflags |

---

## 1. 结论摘要

**新 operator（v1.6.6）从未成功跑过哪怕一次完整的 reconcile（execute）循环。**

代码级证据：`syncInstallation()` 中 target 相关字段的写入是**无条件**的：

- `pkg/virt-operator/kubevirt.go:1033` `util.SetOperatorVersion(kv)` → 写 `status.operatorVersion`
- `pkg/virt-operator/kubevirt.go:1036` `config.SetTargetDeploymentConfig(kv)` → 写 `status.targetKubeVirtVersion` / `targetKubeVirtRegistry` / `targetDeploymentID` / `targetDeploymentConfig`
- `pkg/virt-operator/util/config.go:591-598` `SetTargetDeploymentConfig` 实现
- 目标版本来源：`pkg/virt-operator/util/config.go:218-237` `GetTargetConfigFromKV` 直接读 `kv.Spec.ImageTag`

因此：只要新 operator 对 gen 4 的 KV 执行过一次 execute()，`status.target*` 必然变为 v1.6.6。
**status 仍为 v1.2.0-h3 ⇒ execute() 从未执行 ⇒ operator 的 KV controller worker 没有启动或没有事件触发。**

同理，组件重启后仍是旧镜像，是因为新 operator 从未 apply 新 install strategy
（strategy 是组件清单的唯一来源，见 §3），而非某个 apply 环节失败。

---

## 2. 为什么「重启组件」也没用（install strategy 机制）

virt-operator 更新组件不是直接改 Deployment 镜像，而是：

```
reconcile → loadInstallStrategy() → 找/生成 install strategy configmap → apply configmap 中的清单
```

关键代码：

- `pkg/virt-operator/kubevirt.go:889-984` `loadInstallStrategy()`：
  1. 内存缓存（按 DeploymentID + generation 做 key，`pkg/virt-operator/strategy.go:15-34`）
  2. configmap 缓存查找，匹配条件见 `pkg/virt-operator/resource/generate/install/strategy.go:664-688`：
     `id == config.GetDeploymentID()` **或**（兼容旧路径）`version + registry` 相等
  3. 不命中 → 创建 strategy job（`pkg/virt-operator/strategy_job.go:20-100`）
  4. job 用 `--dump-install-strategy` 把**内置组件清单**写入 configmap
     （`pkg/virt-operator/application.go:162` → `install.DumpInstallStrategyToConfigMap`，
      `pkg/virt-operator/resource/generate/install/strategy.go:355-385`）

**陷阱 1 —— strategy job 的镜像来源**（`pkg/virt-operator/strategy_job.go:22-25`）：

```go
operatorImage := config.VirtOperatorImage      // ← 运行时环境变量 VIRT_OPERATOR_IMAGE
if operatorImage == "" {
    operatorImage = fmt.Sprintf("%s/%s%s%s", config.GetImageRegistry(), config.GetImagePrefix(), VirtOperator, components.AddVersionSeparatorPrefix(config.GetOperatorVersion()))
}
```

`VIRT_OPERATOR_IMAGE` 是镜像构建时注入的环境变量。如果自研镜像的构建流程只改了
Deployment 的 `image` 字段而没注入/更新该 env，strategy job 会用错误镜像 dump 出错误清单，
configmap 的 DeploymentID 与目标不匹配 → job 被删除重建（`kubevirt.go:928-964` 的
"Job failed to create install strategy" 循环）。**该循环是无限重试且每次失败都会等 job 完成，
是「组件永不升级」的又一候选机制。**

**陷阱 2 —— spec.imageTag 会绕过 shasum 校验**（`pkg/virt-operator/util/config.go:334-340`）：

```go
if tag == "" {
    tag = tagFromOperator
} else {
    skipShasums = true   // spec.imageTag 非空时，跳过 shasum 环境变量
}
```

对自研镜像（tag 里带 `-h3` 这类定制后缀）影响有限，但记录在案。

---

## 3. execute() 为什么没跑 —— 三个候选根因（按概率排序）

### 候选 A（最可能）：WaitForCacheSync 被 instancetype v1beta1 informer 卡死

- `pkg/virt-operator/kubevirt.go:762-766` `Run()`：

  ```go
  cache.WaitForCacheSync(stopCh, c.hasSynced)
  // Start the actual work
  ```

  `hasSynced` 要求 **26 个 informer 全部同步**（`pkg/virt-operator/kubevirt.go:169-197`）。

- operator 注册了 instancetype informer（`pkg/virt-operator/application.go:194-195`），
  其实现**硬编码 v1beta1**（`pkg/controller/virtinformers.go:926-944`）：

  ```go
  lw := cache.NewListWatchFromClient(f.clientSet.GeneratedKubeVirtClient().InstancetypeV1beta1().RESTClient(), ...)
  ```

  其余 instancetype informer（`VirtualMachineInstancetype` / `VirtualMachinePreference`）
  同样硬编码 v1beta1（同文件 :916-938）。

- 生产集群是从 v1.2.0 带起来的，其 instancetype CRD 可能只 serve v1alpha1。
  对只 serve v1alpha1 的 CRD 发 v1beta1 List/Watch → 404 → reflector 失败重试 →
  **HasSynced 永假 → WaitForCacheSync 永不返回 → execute() 永不执行**。
  症状与观察完全吻合：无 reconcile 日志、target* 不变、组件不动。

- 这与 fa04d0f8 修复的是**同一类问题**，但 fa04d0f8 只覆盖了
  virt-api / virt-controller / virt-exportproxy 的 snapshot/export/clone，
  **没有覆盖 virt-operator 自身的 instancetype informer**（见 §5）。

- 已有的修复范式可复用：`pkg/virt-config/configuration.go:141-190`（`isSnapshotCrd` /
  `crdServesVersion` / `crdAddedDeleted` 过滤）与 `:431-460`（`HasSnapshotAPI` 等
  同时校验 CRD 存在 + serve 目标版本）。

### 候选 B：Leader election 未成功

- `pkg/virt-operator/application.go:393-414`：拿到 leader 才启动 controller
  （`OnStartedLeading` 里 `go app.kubeVirtController.Run(...)`）。
- 两个 operator Pod 只有一个能拿到 leader。若拿到 leader 的 Pod 恰好卡在
  WaitForCacheSync（候选 A），或 lease 异常导致反复抢主，症状相同。

### 候选 C：新 operator Pod CrashLoop 未被察觉

- 启动路径有多处 `golog.Fatal`（`pkg/virt-operator/application.go:119-243`：
  metrics / hostname / VerifyEnv / client 创建 / namespace 解析 / CRD 探测等）。
- 观察 `kubectl get po` 时可能恰逢 Running 窗口，实际处于 CrashLoopBackOff。

---

## 4. 待验证命令（按顺序执行，一条即可区分三个候选）

```bash
# 1. operator 日志——区分 A/B/C 的决定性证据
kubectl logs -n kubevirt deploy/virt-operator --tail=100 | grep -E "Started leading|Handling KubeVirt|Attempting to acquire|Operator image"

# 2. instancetype CRD 当前 serve 的版本——验证候选 A
kubectl get crd virtualmachineclusterinstancetypes.instancetype.kubevirt.io \
  -o jsonpath='{.spec.versions[*].name}'
kubectl get crd virtualmachineclusterpreferences.instancetype.kubevirt.io \
  -o jsonpath='{.spec.versions[*].name}'
# 期望输出包含 v1beta1；若只有 v1alpha1 → 候选 A 坐实

# 3. operator Pod 真实状态——验证候选 C
kubectl get pods -n kubevirt -l kubevirt.io=virt-operator -o wide
kubectl get pods -n kubevirt -l kubevirt.io=virt-operator \
  -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}'

# 4. strategy job 循环证据——验证 §2 陷阱 1
kubectl get jobs -n kubevirt -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.template.spec.containers[*].image}{"\n"}{end}'
kubectl get cm -n kubevirt | grep strategy

# 5. strategy configmap 的 DeploymentID 与 KV status 是否一致
kubectl get kv kubevirt -n kubevirt -o jsonpath='{.status.targetDeploymentID}'
kubectl get cm -n kubevirt -l operator.kubevirt.io -o jsonpath='{range .items[*]}{.metadata.annotations}{"\n"}{end}' | grep -o 'operator.kubevirt.io/install-strategy-identifier[^,]*'
```

判读表：

| 日志特征 | 结论 |
|---|---|
| 无 "Started leading" | 候选 B（leader election） |
| 有 "Started leading"，无 "Handling KubeVirt resource" | 候选 A（WaitForCacheSync 卡死） |
| 日志含 fatal/panic 且重启计数高 | 候选 C（CrashLoop） |
| 有 "Handling KubeVirt resource" 但反复出现 "Created job" / "Job failed" | §2 陷阱 1（strategy job 镜像错误循环） |

---

## 5. fa04d0f8 与本问题的关系

fa04d0f8「跳版本升级时 snapshot/export/clone v1beta1 informer 条件创建」修复范围：

- `pkg/virt-config/configuration.go`：`HasSnapshotAPI` / `HasExportAPI` / `HasCloneAPI`
  改为同时校验 CRD 存在且 serve v1beta1；`crdAddedDeleted` 过滤中加入 snapshot/export/clone
- `pkg/virt-api/api.go`：vmRestoreInformer 条件创建
- `pkg/virt-controller/watch/application.go`、`pkg/virt-exportproxy`：同类条件创建

**未覆盖**：`pkg/controller/virtinformers.go` 中 instancetype 系列 informer 的硬编码 v1beta1
（:916-944），以及 virt-operator 对它们的注册（`pkg/virt-operator/application.go:194-195`）。
virt-operator 的 hasSynced 依赖这些 informer，卡死位置在 `pkg/virt-operator/kubevirt.go:764`。

---

## 6. 后续修复计划（待验证确认后执行）

1. **（若候选 A 坐实）instancetype informer 版本自适应**：
   - 参照 `pkg/virt-config/configuration.go` 的 CRD 探测模式，在
     `pkg/controller/virtinformers.go:916-944` 按 CRD serve 版本选择
     v1beta1 / v1alpha1，不服务时退回 dummy informer（`testutils.NewFakeInformerFor`
     模式，见 fa04d0f8 的 virtinformers.go 改动）
   - 或先 apply v1.6.6 的 instancetype CRD 清单让 CRD serve v1beta1，再重启 operator
     （最干净的运维路径）
2. **构建流程补版本注入**：`status.operatorVersion` 为
   `v0.0.0-master+$Format:%h$`，说明镜像构建未注入 ldflags
   （默认值在 `staging/src/kubevirt.io/client-go/version/base.go:23`）。
   构建时加 `-ldflags "-X kubevirt.io/client-go/version.gitVersion=v1.6.6"`，
   否则 operatorVersion 永远显示占位符，干扰升级判断。
3. **验证 strategy job 镜像**：确认自研镜像构建流程是否同步注入
   `VIRT_OPERATOR_IMAGE`（`pkg/virt-operator/util/config.go:48`），
   否则 strategy job 会用错误镜像（§2 陷阱 1）。
4. **升级动作修正**：跨主版本跳升（1.2.0→1.6.6）建议直接替换 operator Deployment
   （delete 后 apply 新 yaml），并在新 operator 确认 reconcile 后再 patch KV；
   避免依赖 12-24h 的 informer resync
   （`pkg/controller/virtinformers.go:1142-1147` 的 `ResyncPeriod(12 * time.Hour)`）
   来触发。patch imageTag 仅在 operator 正常 reconcile 的前提下才有效。

---

## 7. 代码位置索引

| 机制 | 位置 |
|---|---|
| execute() 入口 / 状态更新 | `pkg/virt-operator/kubevirt.go:798-887` |
| target* 无条件写入 | `pkg/virt-operator/kubevirt.go:1033-1036` + `pkg/virt-operator/util/config.go:591-598` |
| 目标版本来源（spec.imageTag） | `pkg/virt-operator/util/config.go:218-237`（`:234` 读 ImageTag） |
| imageTag 非空跳过 shasum | `pkg/virt-operator/util/config.go:334-340` |
| WaitForCacheSync 阻塞点 | `pkg/virt-operator/kubevirt.go:762-766` |
| hasSynced 26 个 informer 清单 | `pkg/virt-operator/kubevirt.go:169-197` |
| instancetype informer 硬编码 v1beta1 | `pkg/controller/virtinformers.go:916-944` |
| operator 注册 instancetype informer | `pkg/virt-operator/application.go:194-195` |
| loadInstallStrategy 三级查找 | `pkg/virt-operator/kubevirt.go:889-984` |
| strategy 缓存 key（DeploymentID+generation） | `pkg/virt-operator/strategy.go:15-34` |
| strategy job 镜像来源 | `pkg/virt-operator/strategy_job.go:22-25` |
| strategy configmap 匹配条件 | `pkg/virt-operator/resource/generate/install/strategy.go:664-688` |
| DeploymentID 计算（sha1 over 全字段） | `pkg/virt-operator/util/config.go:724-772` |
| job 失败 → 删除重建循环 | `pkg/virt-operator/kubevirt.go:928-964` |
| leader election 启动 controller | `pkg/virt-operator/application.go:393-414` |
| 启动路径 golog.Fatal 点 | `pkg/virt-operator/application.go:119-243` |
| operatorVersion 占位符来源 | `staging/src/kubevirt.io/client-go/version/base.go:23` |
| informer resync 12-24h | `pkg/controller/virtinformers.go:1142-1147` |
| fa04d0f8 的 CRD 版本探测范式（复用参考） | `pkg/virt-config/configuration.go:141-190, 431-460` |
