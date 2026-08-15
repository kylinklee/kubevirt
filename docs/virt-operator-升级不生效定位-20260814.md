# virt-operator 升级不生效定位：v1.2.0 → v1.6.6 组件未跟随升级

> 定位日期：2026-08-14（初始定位）
> 最终结论日期：2026-08-15（生产环境详细验证后修正）
> 分支：upgrade-v1.2-to-v1.6.6（基于 v1.6.6 + 自研镜像定制）
> 状态：**根因已由生产验证确认：镜像未正确注入版本信息（ldflags gitVersion）。**
> 此前文档中的所有其它定位结论（候选 A/B/C 等）均已被生产验证证伪，仅作过程记录保留，见 §7。

---

## 0. 问题现象

生产环境从 v1.2.0 升级到 v1.6.6，操作分两步：

1. 将 `kubevirt-operator.yaml` 中镜像改为 v1.6.6 并 apply → virt-operator 滚动升级完成
2. `kubectl patch kv kubevirt -n kubevirt --type=json -p '[{ "op": "add", "path": "/spec/imageTag", "value": "v1.6.6" }]'` → **没有任何组件自动升级**

伴随现象：

- 两个 virt-operator Pod 均已是 v1.6.6 镜像
- `kubectl delete po -n kubevirt --all` 重拉全部组件后，除 operator 外组件仍是 v1.2.0-h3
- KV status 中 `operatorVersion: v0.0.0-master+$Format:%h$`（构建占位符，非真实版本）
- KV status 中 `targetKubeVirtVersion` / `observedKubeVirtVersion` 仍为 `v1.2.0-h3`

---

## 1. 根因结论（2026-08-15 生产验证确认）

**唯一根因：自研镜像构建时未向二进制注入版本信息（`-X kubevirt.io/client-go/version.gitVersion` 缺失），
导致 `version.Get()` 返回构建占位符 `v0.0.0-master+$Format:%h$`。**

生产验证结论：将版本信息正确注入镜像后，所有问题现象消失——
第二步 patch `spec.imageTag` 后组件正常自动滚动升级。

直接的代码证据（生产 KV status 可见）：

- `status.operatorVersion` 由运行中的 operator 每次 reconcile 通过
  `version.Get().String()` 写入（`pkg/virt-operator/kubevirt.go:1033` →
  `pkg/virt-operator/util/client.go:191-192`）
- 占位符 `v0.0.0-master+$Format:%h$` 是 `staging/src/kubevirt.io/client-go/version/base.go:23`
  中 `gitVersion` 的编译默认值——出现该值即证明构建时 ldflags 未注入成功

> 说明：此前的机理推断（WaitForCacheSync 卡死等）均与生产验证不符，已证伪并移入 §7 过程记录。
> 若需要把「占位符版本如何具体阻断升级流程」的机理补充进本文档，请基于生产验证时的
> 日志/事件证据追加，不要依据代码推断。

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
| **与本次根因的关系** | **本次根因所在（gitVersion 未注入）** | 与本次问题无关 |

### 2.2 构建期 ldflags 注入（版本信息）——本次根因所在

默认值在 `staging/src/kubevirt.io/client-go/version/base.go:23`：

```go
gitVersion = "v0.0.0-master+$Format:%h$"
```

生产 KV status 里 `operatorVersion: v0.0.0-master+$Format:%h$` 正是这个默认值。

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

**「构建目录有 tag 但未注入」的原因**：

1. **`hack/dockerized` 会排除 `.git`**（:180 `--exclude ".git"`）：上游构建容器内
   没有 git 仓库/tag → `git describe` 失败 → `KUBEVIRT_GIT_VERSION` 为空 →
   `-X gitVersion` 整段跳过。只有官方发布流程
   （`git archive` + `$Format:%D$` 魔数，`version.sh:39-46`）或显式设
   `KUBEVIRT_GIT_VERSION` 才能拿到值；
2. **tag 匹配规则**：`git describe --match='v[0-9]*'`（`version.sh:57`）只认
   `v`+数字开头的可达 tag；
3. **`DOCKER_TAG` 与 ldflags 是两条独立链路**：`hack/config-default.sh:29`
   `docker_tag=${DOCKER_TAG:-latest}` 只决定镜像 tag 后缀（自研的 `-h3` 即来源于此），
   不参与 `version_x_defs` 的 stamp；
4. **bazel 镜像构建不依赖版本 stamp**：`hack/bazel-build-images.sh` 注释明确
   "vars are uninteresting for the build step"——镜像内二进制带占位符是默认行为。

**v1.2.0 与 v1.6.6 的机制完全一致，无差异**（逐文件 diff 验证）：

- `hack/version.sh` 两版本完全相同（含 `get_version_vars` 的 tag 推导与 semver 校验）
- `hack/build-go.sh` 的 `-ldflags "$(kubevirt::version::ldflags)"` 挂载方式相同
- `staging/src/kubevirt.io/client-go/version/base.go:23` 占位符相同
- Bazel stamp 链路相同：`.bazelrc:6` `--stamp --workspace_status_command=./hack/print-workspace-status.sh`
  → `print-workspace-status.sh:43` 输出 `gitVersion ${KUBEVIRT_GIT_VERSION-}`
  → `cmd/virt-operator/BUILD.bazel:23` `x_defs = version_x_defs()`

**结论**：版本注入是官方发布流程专属动作，任何自研构建（无论 1.2.0 还是 1.6.6）
都必须显式设置 `KUBEVIRT_GIT_VERSION`。

### 2.3 自研构建的修复方法

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

### 2.4 如何验证注入是否成功

**端到端验证（最方便）**——`status.operatorVersion` 就是干这个用的：

```bash
kubectl get kv kubevirt -n kubevirt -o jsonpath='{.status.operatorVersion}'
```

显示 `v1.6.6` 而非 `v0.0.0-master+$Format:%h$` ⇒ 注入成功。

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

---

## 3. 运行期 env 注入（VIRT_OPERATOR_IMAGE 等）

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

**发布 yaml 的运维约束**：`kubevirt-operator.yaml.in` 生成的 Deployment 中
`spec.template.spec.containers[].image` 与 env `VIRT_OPERATOR_IMAGE` 必须**同时改**。
只改 image 不改 env 时：operator 主进程是新镜像（RollingUpdate 生效），
但它创建 strategy job 时用的 `VIRT_OPERATOR_IMAGE` 仍是旧镜像 → dump 出旧清单 →
configmap DeploymentID 与目标不匹配 → 组件不升级。

> 注：此机制是客观存在的代码行为，但**不是本次问题的根因**（本次根因见 §1）。
> 保留在此作为发布流程的检查项。

其他同族 env（`pkg/virt-operator/util/config.go:40-81`）：
`VIRT_API_IMAGE` / `VIRT_CONTROLLER_IMAGE` / `VIRT_HANDLER_IMAGE` / `VIRT_LAUNCHER_IMAGE` /
`VIRT_EXPORTPROXY_IMAGE` / `VIRT_EXPORTSERVER_IMAGE` / 各 `*_SHA` shasum 变量（已弃用，
新全镜像变量存在时忽略 shasum，`deployments.go:878-920` `generateVirtOperatorEnvVars`）。

---

## 4. 修复后的升级操作要点

1. 用注入版本信息后的镜像替换 virt-operator Deployment（image 与
   `VIRT_OPERATOR_IMAGE` 同步更新，见 §3）；
2. 确认 `status.operatorVersion` 变为真实版本（`v1.6.6` 或带 commit 后缀的 semver）；
3. 第二步 patch `spec.imageTag: v1.6.6`（或直接由 operator 按新镜像 tag 触发）；
4. 观察 KV status 的 `targetKubeVirtVersion` → 组件滚动升级。

---

## 5. 代码位置索引

| 机制 | 位置 |
|---|---|
| **根因：gitVersion 占位符默认值** | `staging/src/kubevirt.io/client-go/version/base.go:23` |
| operatorVersion 写入 status | `pkg/virt-operator/kubevirt.go:1033` + `pkg/virt-operator/util/client.go:191-192` |
| 版本 ldflags 注入函数与优先级 | `hack/version.sh:47-92`（get_version_vars）、`104-131`（ldflags） |
| 构建脚本挂载 ldflags | `hack/build-go.sh:65-85` |
| dockerized 构建排除 .git（tag 推导失败主因） | `hack/dockerized:180` |
| DOCKER_TAG 默认值（与 ldflags 无关） | `hack/config-default.sh:29` |
| Bazel stamp 链路 | `.bazelrc:6` → `hack/print-workspace-status.sh:43` → `cmd/virt-operator/BUILD.bazel:23` |
| operator Deployment env 生成（VIRT_OPERATOR_IMAGE） | `pkg/virt-operator/resource/generate/components/deployments.go:604-607` |
| VIRT_OPERATOR_IMAGE 读取链 | `pkg/virt-operator/util/config.go:284-349` |
| strategy job 镜像来源 | `pkg/virt-operator/strategy_job.go:22-25` |
| 目标版本来源（spec.imageTag） | `pkg/virt-operator/util/config.go:218-237`（:234 读 ImageTag） |
| target* 写入 status | `pkg/virt-operator/kubevirt.go:1033-1036` + `pkg/virt-operator/util/config.go:591-598` |

---

## 6. 待办

- [ ] 自研构建流水线加入 `KUBEVIRT_GIT_VERSION`（及 commit/treeState）注入，并加
      `go version -m` 校验步骤，防止回归
- [ ] 确认发布 yaml 生成流程中 image 与 `VIRT_OPERATOR_IMAGE` 同步更新（§3 检查项）
- [ ] 若需要，基于生产验证时的日志/事件证据补充「占位符版本阻断升级」的精确机理

---

## 7. 过程记录：被证伪的定位推断（勿再作为依据）

> 以下推断均由 2026-08-15 生产验证证伪。保留在此是为了记录排查路径，
> 防止后续排障重走弯路。**唯一根因见 §1。**

### 7.1 证伪：「WaitForCacheSync 被 instancetype v1beta1 informer 卡死」（候选 A）

推断内容：v1.2.0 集群的 instancetype CRD 只 serve v1alpha1 → v1.6.6 operator 的
instancetype informer 硬编码 v1beta1（`pkg/controller/virtinformers.go`）→ List/Watch
404 → `WaitForCacheSync`（`kubevirt.go:764`）永不返回 → execute() 从未执行。

证伪依据：2026-08-15 生产环境详细验证——注入版本信息后升级即恢复正常，
与 informer/CRD 版本无关。基于该推断实施的两个代码修改
（`c4ae4b9363` instancetype informer 条件创建、`b79744e1e2` configModifiedCallback
注册顺序）**是否保留/回滚待评估**，不在本文档决定。

### 7.2 证伪：「leader election 未成功」（候选 B）

此前已由用户排除（日志确认拿到 leader）。

### 7.3 证伪：「新 operator CrashLoop」（候选 C）

此前已由用户排除（Pod 稳定运行）。

### 7.4 证伪：「execute() 从未执行」的推论链

推断内容：status.target* 仍为旧版本 ⇒ execute() 从未执行（因为
`syncInstallation` 无条件写 target*）。

证伪说明：该推断的前提（execute 的运行方式/触发时机）与生产实际不符。
target* 未更新的真实原因同样是版本信息未注入，具体机理以生产验证证据为准。

### 7.5 教训

1. KV status 的 `operatorVersion` 是构建健康度的第一指示器——出现
   `v0.0.0-master+$Format:%h$` 占位符时，应**先**排查构建注入，再怀疑代码逻辑；
2. 基于代码路径的推断（即使每步都有代码支持）不能替代生产日志/事件证据，
   推断必须经过环境验证才能作为结论；
3. 跳版本升级排障的第一步检查清单应包含：operatorVersion 占位符、
   strategy job 镜像、CRD serve 版本、reconcile 日志。
