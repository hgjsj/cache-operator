# CacheCluster Operator 需求文档

## 1. 项目概述

CacheCluster Operator 是一个用于练习 Kubernetes CRD 与 Operator 开发的示例项目。用户通过创建一个 `CacheCluster` 自定义资源，声明缓存集群的副本数、镜像、存储和资源配置，Operator 负责将期望状态转换为 Kubernetes 原生资源，并持续同步运行状态。

本项目的重点是理解以下机制：

- CRD 的 API 设计
- Controller 的 Reconcile 控制循环
- Kubernetes 原生资源的编排
- OwnerReference 与级联删除
- 自定义资源 Status 和 Conditions
- StatefulSet、Service、PVC、PDB 的组合使用
- 使用 envtest 和 kind 进行测试

## 2. 背景与问题

直接手工部署缓存实例时，需要分别维护 StatefulSet、Service、PVC、ConfigMap 和 PodDisruptionBudget 等资源。资源之间存在引用关系，副本数、镜像和存储配置变更时也需要同步修改多个 YAML 文件。

本项目希望提供一个更高层次的声明式接口：用户只描述缓存集群的期望状态，Operator 自动创建和维护底层资源，并在 `CacheCluster.status` 中反馈当前状态。

## 3. 目标

### 3.1 MVP 目标

1. 定义 `cache.example.com/v1alpha1` API Group 和 `CacheCluster` Kind。
2. 支持用户配置副本数、容器镜像、持久化存储和容器资源。
3. Operator 自动创建和维护以下资源：
   - StatefulSet
   - Headless Service
   - PVC（通过 StatefulSet 的 VolumeClaimTemplate）
   - PodDisruptionBudget
4. 当 `CacheCluster.spec` 发生变化时，底层资源能够收敛到新配置。
5. 通过 OwnerReference 实现资源的级联清理。
6. 在 Status 中反映期望副本数、就绪副本数和基本运行阶段。
7. 为核心 Reconcile 逻辑提供单元测试和 envtest 测试。

### 3.2 第二阶段目标

1. 增加 `Ready`、`Progressing` 和 `Degraded` Conditions。
2. 处理 StatefulSet 未就绪、Pod 镜像拉取失败等异常状态。
3. 支持暂停同步：`spec.paused: true`。
4. 支持配置变更触发滚动更新。
5. 增加更明确的事件和结构化日志。
6. 使用 kind 或 K3s 完成部署验证。

### 3.3 后续扩展目标

以下能力用于后续练习，不属于 MVP：

- 定时备份 CronJob
- 备份状态和恢复流程
- PVC 扩容
- Secret 管理
- Redis Sentinel 或其他现成 HA 组件集成
- 监控指标和 Prometheus ServiceMonitor
- Webhook 校验和默认值
- 升级策略和版本兼容检查

## 4. 非目标

本项目不实现以下功能：

- 自己实现 Redis 或其他缓存系统的数据复制协议。
- 自己实现 Leader 选举、脑裂处理或数据一致性协议。
- 在 MVP 阶段保证真正的缓存数据高可用。
- 替代生产级 Redis、PostgreSQL 或其他数据库 Operator。
- 通过 Operator 实现跨集群容灾。
- 在没有外部 HA 组件支持的情况下宣称多个 Pod 等价于 HA 集群。

StatefulSet 只负责稳定网络标识、Pod 编排和存储声明。真正的主从复制和故障切换需要缓存软件自身或专用 Operator 提供。

## 5. 用户角色

### 5.1 Operator 使用者

使用者通过 YAML 创建和修改 `CacheCluster`，不需要直接维护 StatefulSet、Service 和 PDB。

### 5.2 Operator 开发者

开发者负责 API 版本演进、Reconcile 逻辑、资源构造、状态更新、测试和发布。

## 6. 资源 API 需求

示例资源：

```yaml
apiVersion: cache.example.com/v1alpha1
kind: CacheCluster
metadata:
  name: demo-cache
  namespace: default
spec:
  replicas: 3
  image: redis:7
  storage:
    size: 10Gi
    storageClassName: local-path
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 512Mi
```

### 6.1 Spec 字段

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `replicas` | `int32` | 否 | 缓存实例数量，默认 1，最小 1 |
| `image` | `string` | 否 | 缓存容器镜像，提供项目默认值 |
| `storage.size` | `resource.Quantity` | 否 | 每个实例的持久化存储大小 |
| `storage.storageClassName` | `string` | 否 | PVC 使用的 StorageClass |
| `resources` | `ResourceRequirements` | 否 | 容器 CPU 和内存 requests/limits |
| `paused` | `bool` | 否 | 为 true 时暂停业务资源同步 |

### 6.2 Status 字段

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `phase` | `string` | `Pending`、`Running`、`Degraded` 或 `Unknown` |
| `replicas` | `int32` | StatefulSet 期望副本数 |
| `readyReplicas` | `int32` | 当前就绪副本数 |
| `observedGeneration` | `int64` | 最近处理的资源 Generation |
| `conditions` | `[]Condition` | 当前资源条件 |

## 7. 功能需求

### 7.1 创建

创建 `CacheCluster` 后，Operator 应在合理时间内创建 StatefulSet、Headless Service 和 PDB。StatefulSet 应使用 `CacheCluster` 的命名空间和名称，并设置 OwnerReference。

### 7.2 更新

修改副本数、镜像或资源配置后，Operator 应更新底层 StatefulSet。修改存储配置时，应遵守 Kubernetes PVC 的扩容限制；对于不支持的缩容操作应报告错误，而不是静默修改。

### 7.3 状态同步

Operator 应根据 StatefulSet 的 `status.readyReplicas` 更新 `CacheCluster.status`。所有期望副本就绪时，`phase` 应为 `Running`，并设置 `Ready=True`。

### 7.4 异常处理

当 StatefulSet 未达到期望状态时，Operator 应保留资源、更新 `Progressing` 或 `Degraded` Condition，并通过 Requeue 或关联资源事件继续观察，而不是频繁创建重复资源。

### 7.5 删除

删除 `CacheCluster` 时，由 OwnerReference 自动删除由其管理的资源。若后续引入备份能力，再考虑通过 Finalizer 实现删除前备份。

## 8. 验收标准

### MVP 验收

- [ ] `make manifests` 能生成 CRD 和 RBAC 清单。
- [ ] `make generate` 能生成 DeepCopy 代码。
- [ ] 安装 CRD 后，`kubectl get cacheclusters` 可用。
- [ ] 创建示例 `CacheCluster` 后，StatefulSet、Service 和 PDB 自动出现。
- [ ] 修改 `spec.replicas` 后，StatefulSet 副本数最终一致。
- [ ] 修改 `spec.image` 后，StatefulSet Pod 模板发生更新。
- [ ] `status.readyReplicas` 能反映 StatefulSet 的就绪副本数。
- [ ] 删除 `CacheCluster` 后，其从属资源被清理。
- [ ] 核心 Reconcile 测试通过。

### 第二阶段验收

- [ ] 资源未就绪时 Status Condition 能说明原因。
- [ ] `spec.paused=true` 时不会继续修改业务资源。
- [ ] 配置变化能够触发预期的滚动更新。
- [ ] 能在 K3s 或 kind 集群中完成安装、创建和卸载演示。

## 9. 技术约束

- Go 版本：1.22.x。
- Kubebuilder：4.3.1。
- controller-runtime：0.19.1。
- Kubernetes 依赖：v0.31.x。
- 目标测试环境：本地 K3s 或 kind。
- 项目模块：`example.com/cache-operator`。
- Go Module 代理：`https://mirrors.aliyun.com/goproxy/,direct`。
