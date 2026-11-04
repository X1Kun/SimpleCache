# SimpleCache

SimpleCache 是一个用 Go 实现的分布式加载式缓存，配套 Kubernetes Operator，以商品 JSON 为例验证读多写少场景下的缓存、条件写入和故障恢复。

**PostgreSQL 保存业务数据和版本，缓存实例按键分担读取压力。** 读取未命中时从数据库加载；写入先提交数据库，再通知各实例清除旧缓存。事务 Outbox 持久化通知事件，让写入实例故障后仍能继续投递。

引擎的服务、压测、诊断和报告入口见 [cmd 角色说明](geecache-engine/cmd/README.md)。

## 整体架构

下面展示可选的 **Gateway 模式：两个代理实例、三个缓存分片、一个共享 PostgreSQL**。

```mermaid
flowchart TB
    client["业务客户端"] --> entry["统一访问入口"]

    subgraph gateway["网关层：Envoy Gateway 管理"]
        proxy0["Envoy 代理 0"]
        proxy1["Envoy 代理 1"]
        routing["各代理按请求 key 选择缓存分片"]
        proxy0 --> routing
        proxy1 --> routing
    end

    entry --> proxy0
    entry --> proxy1

    subgraph cache["缓存层：三个独立分片"]
        svc0["分片 Service 0"] --> pod0["缓存实例 0"]
        svc1["分片 Service 1"] --> pod1["缓存实例 1"]
        svc2["分片 Service 2"] --> pod2["缓存实例 2"]
    end

    routing --> svc0
    routing --> svc1
    routing --> svc2
    pod0 -->|"未命中回源 / 条件写入 / 通知任务"| db
    pod1 --> db
    pod2 --> db
    db[("PostgreSQL<br/>业务数据、版本、Outbox 事件")]

    operator["SimpleCache Operator"]
    operator -.->|"管理网关配置"| gateway
    operator -.->|"管理实例与 Service"| cache
```

图中的分片选择是代理内部逻辑，不是额外服务。每个分片 Service 对应一个缓存 Pod，三个实例保存不同键的缓存，当前没有为每个分片配置缓存副本。代理配置和健康状态一致时，同一个键会路由到同一分片；扩缩容或故障期间路由会逐步收敛。

| 组件 | 职责 |
| --- | --- |
| Envoy 代理 | 接收请求，按键选择分片，支持多个代理实例 |
| 缓存实例 | 提供读写 API；维护本地 LRU/TTL 缓存；合并同键请求并限制回源并发；发送和接收失效通知 |
| PostgreSQL | 保存权威数据和版本，原子检查写入条件，同时记录 Outbox 事件 |
| Outbox 后台任务 | 运行在缓存实例中，领取通知事件并重试；实例故障后由其他实例接续 |
| Operator 与服务发现 | Operator 管理部署资源；缓存实例通过 EndpointSlice 发现失效通知的目标成员 |

实例统一读取本地缓存并在未命中时回源 PostgreSQL。多实例按 key 分片由 Gateway 完成；实例间仅传递版本失效通知。单实例可直接访问 API，无需 Gateway。无 Gateway 的多实例为独立缓存副本。

## 读写如何工作

**读取：** 请求到达对应分片后，优先返回本地缓存；未命中则读取 PostgreSQL 并缓存结果。同键并发请求共享加载任务，容量不足时明确拒绝，避免无限排队和集中回源。

**写入：** 客户端携带读取到的版本提交修改。数据库检查版本，成功后同时更新数据并记录通知事件；版本不匹配则返回 `412`，避免覆盖其他请求已提交的修改。

```mermaid
sequenceDiagram
    participant C as 业务客户端
    participant W as 接收写入的缓存实例
    participant DB as PostgreSQL
    participant N as 已发现的缓存成员
    participant R as 任一实例的 Outbox 任务
    C->>W: 携带版本提交修改
    W->>DB: 同一事务更新数据并记录通知事件
    DB-->>W: 提交成功
    W->>N: 通知清除旧版本缓存
    W-->>C: 返回写入及通知结果
    Note over DB,R: 后台投递独立执行，可与同步通知重叠
    R->>DB: 领取待投递事件
    R->>N: 投递版本通知，失败后重试
    R->>DB: 全部目标确认后标记完成
```

同步通知用于尽快清除旧缓存，Outbox 用于故障后的持续重试。通知可能重复或乱序，缓存会检查版本，避免旧通知清除更新的数据。

**一致性边界：** 这里不提供强一致读，写入后可能短暂读到旧缓存。通知完成只覆盖当次已发现的成员；离线或未发现的旧缓存仍依赖 TTL 过期恢复。一份 Outbox 队列服务于同一个缓存集群。

## 快速开始

### 完整开发环境（首选）

使用独立 Kind 集群启动 Gateway、Operator、带 PVC 的 PostgreSQL 和缓存集群：

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r tools/requirements.txt
make dev-up
make dev-status
curl -fsS 'http://127.0.0.1:18080/api?key=product%3A42'
make dev-down   # 停止业务，保留 Kind 集群、数据库 PVC 和凭据
```

再次 `make dev-up` 恢复；`make dev-schema-upgrade` 显式升级已有库，
`make dev-destroy` 才销毁集群及其数据。完整的依赖、存储、访问和生命周期说明见
[开发部署指南](deploy/system/README.md)。开发入口保留环境，验证入口复用基础定义并在结束后清理。

### 本地单实例

使用兼容 Go 1.25.3 的工具链，在一个终端启动内置演示数据源：

```bash
cd geecache-engine
go run -buildvcs=false ./cmd/simplecache -port=8001 -api=true
```

在另一个终端读取数据：

```bash
curl -fsS 'http://localhost:9999/api?key=Auto-666'
```

### 三实例与 PostgreSQL

在仓库根目录启动 Compose 示例，作为独立本地缓存副本运行：

```bash
docker compose -f deploy/products/compose.yaml up -d --build
curl -fsS http://127.0.0.1:19999/readyz
curl -fsS 'http://127.0.0.1:19999/api?key=product%3A42'
```

使用结束后关闭资源，保留数据库卷：

```bash
docker compose -f deploy/products/compose.yaml down
```

已有数据库卷需要先执行[显式结构升级](docs/design.md#postgresql-example)。条件写入示例、鉴权配置和 Kubernetes / Gateway 部署见[中文设计文档](docs/design.zh-CN.md#run-and-configure)。

## 测试与验证

```bash
make check                         # Go 格式、测试、race、构建和静态检查
make test-operator-ci              # Operator 测试
make gateway-smoke                 # 真实 Gateway 与 PostgreSQL 集群冒烟验证
make gateway-experiments           # 并发、通知丢失与故障恢复场景
make gateway-perf                  # 随机路由与哈希路由的对照实验
```

测试覆盖版本冲突、旧响应晚到、重复和乱序通知、提交后崩溃、Outbox 重放及过载保护。集群实验使用独立的临时 Kind 环境，报告保存在 `bin/reports/`。功能测试通过不代表已经测得容量上限，具体证据与限制见[验证说明](docs/design.zh-CN.md#validation-and-evidence)。

## 目录与文档

| 目录 | 内容 |
| --- | --- |
| `geecache-engine/` | 缓存引擎、数据源、通知任务及 Go 测试和压测工具 |
| `simplecache-operator/` | Kubernetes API、控制器和部署资源 |
| `deploy/` | 本地 Compose、Kubernetes、Gateway 与监控示例 |
| `tools/validation/` | 集群生命周期、功能与故障实验、性能测量和报告 |
| `scripts/` | 开发脚本与兼容入口 |
| `docs/` | 设计、配置、验证证据和后续计划 |
| `.github/workflows/` | 持续集成检查 |

深入阅读：[中文设计文档](docs/design.zh-CN.md) · [完整设计与操作指南（英文）](docs/design.md) · [引擎模块索引](geecache-engine/internal/README.md) · [Outbox 协议说明](geecache-engine/internal/outbox/README.md)。
