# virt-operator 升级不生效定位：v1.2.0 → v1.6.6 组件未跟随升级

> 定位日期：2026-08-14（更新：排除候选 B/C，候选 A 定为最终结论）
> 分支：upgrade-v1.2-to-v1.6.6（基于 v1.6.6 + 自研镜像定制）
> 状态：**根因已定位（候选 A），待生产验证命令最终坐实后实施代码修复**

---

## 0. 问题现象

生产环境从 v1.2.0 升级到 v1.6.6，操作分两步：

1. 将 `kubevirt-operator.yaml` 中镜像改为 v1.6.6 并 apply → virt-operator 滚动升级完成
2. `kubectl patch kv kubevirt -n kubevirt --type=json -p '[{ "op": "add", "path": "/spec/imageTag", "value": "v1.6.6" }]'` → **没有任何组件自动升级**

已确认的事实：

- 两个 virt-operator Pod 均已是 v1.6.6 镜像
- 发现不升级后执行 `kubectl delete po -n kubevirt --all` 重拉全部组件 → **除 operator 外组件仍是 v1.2.0-h3**
- fa04d0f8 已修复 snapshot/export/clone 的 v1beta1 informer 兼容问题（与本问题同类但不同范围，见 §5）
- **候选 B（leader election 未成功）与候选 C（新 operator CrashLoop）已排除**：
  operator 日志确认拿到了 leader 且 Pod 稳定运行（非 CrashLoop），
  但**从未出现 "Handling KubeVirt resource" reconcile 日志**

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
| `status.operatorVersion` | `v0.0.0-master+$Format:%h$` | 镜像构建未注入版本 ldflags（见 §6.2） |

---

## 1. 根因结论

**virt-operator（v1.6.6）的 controller worker 因 WaitForCacheSync 被 instancetype v1beta1 informer
卡死而从未启动，导致 execute() 一次都没有执行过。**

完整的因果链：

```
生产集群由 v1.2.0 带起 → instancetype CRD（virtualmachineclusterinstancetypes 等）只 serve v1alpha1
→ v1.6.6 的 virt-operator 注册的 instancetype informer 硬编码 v1beta1（virtinformers.go:916-944）
→ 对只 serve v1alpha1 的 CRD 发 v1beta1 List/Watch → 404 → reflector 失败重试
→ 该 informer HasSynced 永假
→ kubevirt.go:764 cache.WaitForCacheSync(stopCh, c.hasSynced) 永不返回（hasSynced 要求 26 个 informer 全部同步）
→ runWorker 从未启动（kubevirt.go:775-790）
→ execute() 从未执行
→ 不读 spec.imageTag、不写 status.target*、不创建 strategy job、不 apply 新组件清单
→ 组件永远停留在 v1.2.0-h3，重启也无用
```

### 1.1 为什么「execute() 从未执行」是铁证

`syncInstallation()` 中 target 相关字段的写入是**无条件**的：

- `pkg/virt-operator/kubevirt.go:1033` `util.SetOperatorVersion(kv)` → 写 `status.operatorVersion`
- `pkg/virt-operator/kubevirt.go:1036` `config.SetTargetDeploymentConfig(kv)` → 写 `status.targetKubeVirtVersion` / `targetKubeVirtRegistry` / `targetDeploymentID` / `targetDeploymentConfig`
- `pkg/virt-operator/util/config.go:591-598` `SetTargetDeploymentConfig` 实现
- 目标版本来源：`pkg/virt-operator/util/config.go:218-237` `GetTargetConfigFromKV` 直接读 `kv.Spec.ImageTag`

只要新 operator 对 gen 4 的 KV 执行过一次 execute()，`status.target*` 必然变为 v1.6.6。
**status 仍为 v1.2.0-h3 ⇒ execute() 从未执行。**

### 1.2 为什么「重启组件」也没用（install strategy 机制）

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

execute() 从未执行 → strategy configmap 从未刷新为 v1.6.6 清单 →
即使组件 Pod 被删重启，Deployment/DaemonSet 的定义仍是 v1.2.0-h3 镜像 → 拉起的还是旧版本。

### 1.3 与 fa04d0f8 的关系

本问题与 fa04d0f8 是**同一类问题（跳版本升级时硬编码 v1beta1 informer 在只 serve v1alpha1 的
CRD 上 404）**，但 fa04d0f8 只覆盖了 virt-api / virt-controller / virt-exportproxy 的
snapshot/export/clone，**没有覆盖 virt-operator 自身的 instancetype informer**（见 §5 代码位置对比）。

---

## 2. 环境变量注入镜像的机制与方法

### 2.1 两种注入方式的区别

| | 构建期注入（ldflags） | 运行期注入（Deployment env） |
|---|---|---|
| 注入对象 | Go 程序内的包级变量 | 容器环境变量（`os.Getenv` 读取） |
| 机制 | `go build -ldflags "-X import.path.var=value"` 在链接期改写字符串变量 | Kubernetes 在容器启动时设置 env，由 Deployment 模板生成 |
| 典型用途 | 版本号（gitVersion/gitCommit/buildDate） | 镜像全名（VIRT_OPERATOR_IMAGE）、shasum、目标 namespace 等 |
| 生效时机 | 二进制编译完成后即固定 | Pod 每次创建时 |
| 本仓库的载体 | `hack/version.sh` 的 `kubevirt::version::ldflags` | `pkg/virt-operator/resource/generate/components/deployments.go` |

### 2.2 构建期 ldflags 注入（版本信息）

默认值在 `staging/src/kubevirt.io/client-go/version/base.go:23`：

```go
gitVersion = "v0.0.0-master+$Format:%h$"
```

生产 KV status 里 `operatorVersion: v0.0.0-master+$Format:%h$` 正是这个默认值，
说明自研镜像构建时没有注入 `gitVersion`。

**上游的「默认注入」机制**：`hack/build-go.sh:65-85` 的所有构建目标都带
`-ldflags "$(kubevirt::version::ldflags)"`，其中 gitVersion 的值按以下优先级决定
（`hack/version.sh:47-92` `get_version_vars()`）：

1. 显式环境变量 `KUBEVIRT_GIT_VERSION`（上游发布流程的正式途径）；
2. 否则用 `git describe --match='v[0-9]*' --tags --abbrev=14` 从 git tag 自动推导
   （tag 距离以 `+N+hash` 形式编入 semver，脏树追加 `-dirty`）；
3. 两条路都走不通（构建目录是 `git archive` 导出且无 tag、或根本不是 git 仓库）时
   `KUBEVIRT_GIT_VERSION` 为空 → `ldflags()` 中对应 `-X ...gitVersion=...` 整段跳过
   （`hack/version.sh:119-121`）→ 二进制保留 `base.go` 占位符。

另外 `version.sh:40-44` 会校验推导结果是否符合 semver，不符合则**拒绝构建**。

自研镜像出现占位符 ⇒ 构建流程没有经过带 tag 的 git 目录且未显式设置
`KUBEVIRT_GIT_VERSION`。想让 operatorVersion 精确显示 `v1.6.6`，显式设置
`KUBEVIRT_GIT_VERSION=v1.6.6` 是唯一可靠做法。

上游注入实现（`hack/version.sh:104-131`）：

```bash
function kubevirt::version::ldflags() {
    kubevirt::version::get_version_vars
    version_pkg="kubevirt.io/client-go/version"
    local -a ldflags=($(kubevirt::version::ldflag ${version_pkg} "buildDate" "..."))
    if [[ -n ${KUBEVIRT_GIT_COMMIT-} ]]; then
        ldflags+=($(kubevirt::version::ldflag ${version_pkg} "gitCommit" "${KUBEVIRT_GIT_COMMIT}"))
        ...
    fi
    if [[ -n ${KUBEVIRT_GIT_VERSION-} ]]; then
        ldflags+=($(kubevirt::version::ldflag ${version_pkg} "gitVersion" "${KUBEVIRT_GIT_VERSION}"))
    fi
    echo "${ldflags[*]-}"
}
```

构建脚本统一使用（`hack/build-go.sh:65-85`，所有 go build 目标都带
`-ldflags "$(kubevirt::version::ldflags)"`）：

```bash
go ${target} -v -tags "${KUBEVIRT_GO_BUILD_TAGS}" \
    -ldflags "$(kubevirt::version::ldflags)" ... ./cmd/...
```

自研构建的两种修复方法：

1. **沿用上游机制**：构建时设置 `KUBEVIRT_GIT_VERSION=v1.6.6`（及
   `KUBEVIRT_GIT_COMMIT`/`KUBEVIRT_GIT_TREE_STATE`）再执行 hack/build-go.sh；
2. **直接手工注入**（不依赖 hack 脚本时）：

   ```bash
   go build -ldflags "-X kubevirt.io/client-go/version.gitVersion=v1.6.6 \
                       -X kubevirt.io/client-go/version.gitCommit=$(git rev-parse --short HEAD)" \
       ./cmd/virt-operator
   ```

   注意包路径必须是 `kubevirt.io/client-go/version`（staging 目录经
   vendor 链接后以该 import path 参与编译，见 `pkg/virt-operator/util/client.go:35` 的 import）。

### 2.2.1 如何验证注入是否成功

**端到端验证（最方便）**——`status.operatorVersion` 就是干这个用的：

```bash
kubectl get kv kubevirt -n kubevirt -o jsonpath='{.status.operatorVersion}'
```

它由运行中的 operator 每次 reconcile 通过 `version.Get().String()` 写入
（`pkg/virt-operator/kubevirt.go:1033`）。显示 `v1.6.6` 而非
`v0.0.0-master+$Format:%h$` ⇒ 注入成功**且** operator 在正常 reconcile
（因此这也是验证候选 A 修复成效的指标之一）。

**本地二进制验证（构建后立刻查，无需运行）**——`go version -m` 读嵌入的
build 信息，ldflags 注入的所有变量都在 `-build settings` 里：

```bash
go version -m ./_out/cmd/virt-operator/virt-operator
# 关键行：
#   build -ldflags=" -X kubevirt.io/client-go/version.gitVersion=v1.6.6 ..."
#   build vcs.revision=6cdc33d7832493
```

镜像里同样适用（先拉镜像或直接在 Pod 内执行，路径按实际镜像 layout）：

```bash
kubectl exec -n kubevirt deploy/virt-operator -- go version -m /usr/bin/virt-operator
```

**不要用**：operator 启动日志里的 `Operator image: ...`——它来自运行时环境变量
`VIRT_OPERATOR_IMAGE`，反映不了 ldflags 注入结果。

### 2.3 运行期 env 注入（VIRT_OPERATOR_IMAGE 等）

`VIRT_OPERATOR_IMAGE` 由组件生成器写入 operator Deployment 的容器 env
（`pkg/virt-operator/resource/generate/components/deployments.go:604-607`）：

```go
Env: []corev1.EnvVar{
    {
        Name:  operatorutil.VirtOperatorImageEnvName,   // "VIRT_OPERATOR_IMAGE"
        Value: image,                                    // 生成时的 operator 镜像全名
    },
    ...
},
```

运行时读取链：

```
operator 进程启动
→ pkg/virt-operator/util/config.go:284 GetOperatorImageWithEnvVarManager
   （优先 VIRT_OPERATOR_IMAGE，回退废弃的 OPERATOR_IMAGE）
→ config.go:296-349 getConfig() 用它解析 registry/prefix/tagFromOperator
→ config.VirtOperatorImage 进入 KubeVirtDeploymentConfig
→ strategy_job.go:22-25：strategy job 容器镜像 = config.VirtOperatorImage
→ job --dump-install-strategy 用该镜像 dump 组件清单
```

**发布 yaml 的运维约束**（本问题直接相关）：`kubevirt-operator.yaml.in` 生成的 Deployment
中 `spec.template.spec.containers[].image` 与 env `VIRT_OPERATOR_IMAGE` 必须**同时改**。
只改 image 不改 env 时：operator 主进程是新镜像（RollingUpdate 生效），
但它创建 strategy job 时用的 `VIRT_OPERATOR_IMAGE` 仍是旧镜像 → dump 出旧清单 →
configmap DeploymentID 与目标不匹配 → 进入「Job failed to create install strategy」的
删除-重建循环（`kubevirt.go:928-964`）→ 组件永不升级。

其他同族 env（`pkg/virt-operator/util/config.go:40-81`）：
`VIRT_API_IMAGE` / `VIRT_CONTROLLER_IMAGE` / `VIRT_HANDLER_IMAGE` / `VIRT_LAUNCHER_IMAGE` /
`VIRT_EXPORTPROXY_IMAGE` / `VIRT_EXPORTSERVER_IMAGE` / 各 `*_SHA` shasum 变量（已弃用，
新全镜像变量存在时忽略 shasum，`deployments.go:878-920` `generateVirtOperatorEnvVars`）。

---

## 3. 根因代码位置

### 3.1 卡死点

`pkg/virt-operator/kubevirt.go:762-766` `Run()`：

```go
// Wait for cache sync before we start the controller
cache.WaitForCacheSync(stopCh, c.hasSynced)

// Start the actual work
for i := 0; i < threadiness; i++ {
    go wait.Until(c.runWorker, time.Second, stopCh)
}
```

`hasSynced` 要求 **26 个 informer 全部同步**（`pkg/virt-operator/kubevirt.go:169-197`），
任何一个 HasSynced 永假都会让 worker 永不启动。

### 3.2 出错的 informer（硬编码 v1beta1）

operator 注册了 instancetype informer（`pkg/virt-operator/application.go:194-195`）：

```go
ClusterInstancetype: app.informerFactory.VirtualMachineClusterInstancetype(),
ClusterPreference:   app.informerFactory.VirtualMachineClusterPreference(),
```

其实现硬编码 v1beta1（`pkg/controller/virtinformers.go:926-944`）：

```go
func (f *kubeInformerFactory) VirtualMachineClusterInstancetype() cache.SharedIndexInformer {
    return f.getInformer("vmClusterInstancetypeInformer", func() cache.SharedIndexInformer {
        lw := cache.NewListWatchFromClient(
            f.clientSet.GeneratedKubeVirtClient().InstancetypeV1beta1().RESTClient(),  // ← 硬编码
            instancetypeapi.ClusterPluralResourceName, k8sv1.NamespaceAll, fields.Everything())
        return cache.NewSharedIndexInformer(lw, &instancetypev1beta1.VirtualMachineClusterInstancetype{}, ...)
    })
}
```

其余三个 instancetype informer 同样硬编码 v1beta1（同文件 :916-938）：
`VirtualMachineInstancetype` / `VirtualMachinePreference` / `VirtualMachineClusterPreference`。

### 3.3 为什么 List/Watch 404 会让 HasSynced 永假

对只 serve v1alpha1 的 CRD 发 `.../v1beta1` 的 List/Watch，apiserver 返回 404
（该 CRD 没有 v1beta1 serving 版本）。client-go reflector 对 404 的处理是
**返回错误并退避重试，而不是放弃**：informers 框架中 HasSynced 只会在
首次 List+Watch 成功后才置真。404 永不消失 → HasSynced 永假 →
`WaitForCacheSync` 阻塞 → 无任何 reconcile 日志。这与生产观察完全吻合
（有 "Started leading"，无 "Handling KubeVirt resource"）。

---

## 4. 最终验证命令（坐实根因）

```bash
# 1. instancetype CRD 当前 serve 的版本（决定性证据）
kubectl get crd virtualmachineclusterinstancetypes.instancetype.kubevirt.io \
  -o jsonpath='{.spec.versions[*].name}'
kubectl get crd virtualmachineclusterpreferences.instancetype.kubevirt.io \
  -o jsonpath='{.spec.versions[*].name}'
# 期望：只有 v1alpha1（v1.2.0 时代 CRD）→ 根因坐实

# 2. operator 日志中 reflector 失败的佐证
kubectl logs -n kubevirt deploy/virt-operator --tail=200 | grep -iE "list.*virtualmachineclusterinstancetype|v1beta1.*not found|the server could not find"

# 3. 确认 reconcile 从未发生
kubectl logs -n kubevirt deploy/virt-operator --tail=500 | grep -c "Handling KubeVirt resource"
# 期望：0
```

---

## 5. fa04d0f8 与本问题的关系（代码位置对比）

fa04d0f8「跳版本升级时 snapshot/export/clone v1beta1 informer 条件创建」修复范围：

- `pkg/virt-config/configuration.go:141-190`：`isSnapshotCrd` / `isExportCrd` / `isCloneCrd` /
  `crdServesVersion`（:159-165 检查 `v.Name == version && v.Served`）/
  `crdAddedDeleted` 过滤（:168-190）
- `pkg/virt-config/configuration.go:431-460`：`HasSnapshotAPI` / `HasExportAPI` / `HasCloneAPI`
  同时校验 CRD 存在 + serve v1beta1
- `pkg/controller/virtinformers.go:699-914`：snapshot/export/clone/CDI 的条件创建模式，
  不支持时退回 dummy informer（`testutils.NewFakeInformerFor`）
- `pkg/virt-api/api.go`、`pkg/virt-controller/watch/application.go`、virt-exportproxy：条件创建接线

**未覆盖（本问题的根因）**：`pkg/controller/virtinformers.go:916-944` 的 instancetype
系列 informer 硬编码 v1beta1，以及 virt-operator 对它们的注册
（`pkg/virt-operator/application.go:194-195`）。virt-operator 的 hasSynced 依赖这些
informer，卡死位置在 `pkg/virt-operator/kubevirt.go:764`。

---

## 6. 后续修复计划

### 6.1 instancetype informer 版本自适应（核心修复）

参照 fa04d0f8 的 CRD 探测 + dummy informer 模式，改 `pkg/controller/virtinformers.go:916-944`：

1. 在 `pkg/virt-config/configuration.go` 增加 `HasInstancetypeAPI()`（CRD 存在且 serve
   v1beta1 才返回 true），并把 `isInstancetypeCrd` 加入 `crdAddedDeleted` 过滤
   （:168-190），使 CRD 出现/消失时触发回调重新初始化（模式同 snapshot/export/clone）；
2. `kubeInformerFactory` 持有 clusterConfig 引用（或通过回调），在四个 instancetype
   informer 的构造处按 `HasInstancetypeAPI()` 分支：
   - serve v1beta1 → 现有 v1beta1 ListWatch（保持现状）
   - 否则 → dummy informer：
     ```go
     informer, _ := testutils.NewFakeInformerFor(&instancetypev1beta1.VirtualMachineClusterInstancetype{})
     return informer
     ```
     （dummy 模式参考 `DummyOperatorSCC`，`pkg/controller/virtinformers.go:1220-1225`
     附近同文件既有实现：`testutils.NewFakeInformerFor(&secv1.SecurityContextConstraints{})`）
3. 确保 virt-operator 的 `hasSynced`（`kubevirt.go:169-197`）不受影响——dummy informer
   的 HasSynced 立即为真，operator 得以正常启动 reconcile。

### 6.2 构建流程补版本 ldflags 注入

`status.operatorVersion` 为 `v0.0.0-master+$Format:%h$`，说明自研镜像构建未注入
`gitVersion`。按 §2.2 修复（设置 `KUBEVIRT_GIT_VERSION` 或手工 `-X` 注入），
否则 operatorVersion 永远显示占位符，干扰升级判断与排障。

### 6.3 发布 yaml 的 image 与 VIRT_OPERATOR_IMAGE 一致性

自研镜像发布流程需保证 `kubevirt-operator.yaml` 中 Deployment 的 image 字段与
env `VIRT_OPERATOR_IMAGE` 同步更新（§2.3），否则 strategy job 用错误镜像
dump 出旧清单，陷入「Job failed → 删除 → 重建」循环（`kubevirt.go:928-964`）。

### 6.4 运维路径（修复发布前的临时绕过）

在代码修复发布前，生产可先 apply v1.6.6 的 instancetype CRD 清单使 CRD serve
v1beta1，然后重启 virt-operator Pod——WaitForCacheSync 即可通过，operator
开始正常 reconcile（读 spec.imageTag → 创建 strategy job → apply 新组件清单 →
组件滚动升级）。注意：跨主版本跳升（1.2.0→1.6.6）后建议检查 strategy job 镜像
与 KV status 的 DeploymentID 匹配（见 §4 命令），确认后再观察组件滚动升级。

---

## 7. 代码位置索引

| 机制 | 位置 |
|---|---|
| execute() 入口 / 状态更新 | `pkg/virt-operator/kubevirt.go:798-887` |
| target* 无条件写入 | `pkg/virt-operator/kubevirt.go:1033-1036` + `pkg/virt-operator/util/config.go:591-598` |
| 目标版本来源（spec.imageTag） | `pkg/virt-operator/util/config.go:218-237`（:234 读 ImageTag） |
| imageTag 非空跳过 shasum | `pkg/virt-operator/util/config.go:334-340` |
| **根因卡死点：WaitForCacheSync** | `pkg/virt-operator/kubevirt.go:762-766` |
| hasSynced 26 个 informer 清单 | `pkg/virt-operator/kubevirt.go:169-197` |
| **根因：instancetype informer 硬编码 v1beta1** | `pkg/controller/virtinformers.go:916-944` |
| operator 注册 instancetype informer | `pkg/virt-operator/application.go:194-195` |
| loadInstallStrategy 三级查找 | `pkg/virt-operator/kubevirt.go:889-984` |
| strategy 缓存 key（DeploymentID+generation） | `pkg/virt-operator/strategy.go:15-34` |
| strategy job 镜像来源（VIRT_OPERATOR_IMAGE） | `pkg/virt-operator/strategy_job.go:22-25` |
| operator Deployment env 生成（VIRT_OPERATOR_IMAGE） | `pkg/virt-operator/resource/generate/components/deployments.go:604-607` |
| strategy configmap 匹配条件 | `pkg/virt-operator/resource/generate/install/strategy.go:664-688` |
| DeploymentID 计算（sha1 over 全字段） | `pkg/virt-operator/util/config.go:724-772` |
| job 失败 → 删除重建循环 | `pkg/virt-operator/kubevirt.go:928-964` |
| leader election 启动 controller | `pkg/virt-operator/application.go:393-414` |
| operatorVersion 占位符来源 | `staging/src/kubevirt.io/client-go/version/base.go:23` |
| 版本 ldflags 注入函数 | `hack/version.sh:104-131`（构建脚本 `hack/build-go.sh:65-85`） |
| env 常量与 GetOperatorImage 读取 | `pkg/virt-operator/util/config.go:40-81, 284-296` |
| dummy informer 模式（修复参考） | `pkg/controller/virtinformers.go:1220-1225`（`DummyOperatorSCC`） |
| fa04d0f8 的 CRD 版本探测范式（复用参考） | `pkg/virt-config/configuration.go:141-190, 431-460` |
