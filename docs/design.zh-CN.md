# SimpleCache：架构、运行与路线图（精简中文版）

本文根据 [英文设计文档](design.md) 整理，保留核心设计、运行方式与验证边界，省略重复说明及历史实验过程。2026-10-30 的补充验证见下文报告链接。

当前目录按职责分层：运行时代码位于 `geecache-engine/internal/{app,cache,peer,discovery,source,telemetry}`；Go 压测位于 `internal/bench/{gateway,postgres,diagnostics}`，验证与报告位于 `internal/validation/{scenarios,report}`；Python 集群编排位于 `tools/validation`。部署示例集中在 `deploy`，旧脚本和 Compose 路径保留兼容入口。模块说明见 [Go 工具目录](../geecache-engine/internal/bench/README.md) 和 [集群验证工具](../tools/validation/README.md)。

- [当前实现](#current-implementation)
- [目标架构](#agreed-target-architecture)
- [运行与配置](#run-and-configure)
- [验证与证据](#validation-and-evidence)
- [后续计划](#delivery-plan)

<a id="current-implementation"></a>

## 当前实现

SimpleCache 是使用 Go 实现的加载式缓存，配套 Kubernetes Operator。**PostgreSQL 是数据和版本的唯一权威来源**；缓存只保存可丢弃的有限副本。商品 JSON 用于可重复的读多写少实验，另有不可变 demo 数据源用于诊断。

| 能力 | 当前行为 |
| --- | --- |
| 读取 | 默认 Peer 一致性哈希路由；可选 Gateway 按键分片后直接读取本地缓存或数据库 |
| 写入 | 鉴权后执行数据库原子条件写入，再发送版本失效通知 |
| 缓存 | LRU、TTL、可选短时 hot 副本、分批主动过期清理 |
| 并发保护 | API/Peer miss 等待、共享路由、回源队列及写请求分别限额 |
| 版本保护 | 有界版本下界、epoch 屏障、最多一次旧版本回源重试 |
| 部署 | StatefulSet；可选多代理 Envoy Gateway；每分片一个稳定 Service |
| 一致性 | PostgreSQL 写入与 Outbox 事件原子提交，后台至少一次投递到成功的已知成员快照；不提供强一致读或离线节点补齐 |

### 读取路径与保护

Gateway 模式：

```text
客户端 → Envoy 按规范化 key 做 Maglev 哈希
       → 分片 Service → 对应缓存 Pod API
       → main LRU；未命中 → originFlight → 有界回源 → PostgreSQL
```

该模式使用 `GetDirect`，跳过 `routeFlight` 和 Peer 读取；`HOT_CACHE_BYTES` 必须为 0。缓存节点仍通过 EndpointSlice 发现成员，用于失效通知。

默认 Peer 模式：

```text
API → main/hot LRU
    → 未命中：可选 Bloom → routeFlight → 本地加载或一次 Peer 请求
    → 本地加载：originFlight → 再次查缓存 → 回源限流 → Getter
    → Peer 网络/协议失败：在剩余路由等待时间内回退到本地数据源
```

Peer 接收端只做本地读取，不继续转发。路由和回源使用独立 SingleFlight，避免递归等待。同键共享任务，但每个等待者仍占用等待名额。调用者取消只停止自己的等待，共享任务由服务生命周期和独立截止时间约束；Getter 实现必须响应 context 取消。

| 预算 | 默认值 |
| --- | --- |
| API 读等待 / 共享路由任务 / 单次 Peer 读取 | 1,500 / 1,200 / 1,000 ms |
| 回源任务，含等待执行槽位 | 800 ms |
| 每进程执行中的 Getter / 回源排队任务 | 32 / 128 |
| API miss / Peer miss / 共享路由任务 | 各 128 |
| 写入与仅失效请求 | 并发 16 |
| API/Peer 响应写超时 | 10 秒 |

命中和探针不排在 miss 请求后等待。队列满时明确拒绝，过载返回 `503` 和 `Retry-After: 1`。Peer 明确报告 `OVERLOADED` 时不再本地回源；只有带 `KEY_NOT_FOUND` 标记的 Peer 404 才是明确的业务未命中，其他 Peer 错误按规则回退。

旧版本回源结果最多重试一次，沿用原 800 ms 截止时间和正常回源限流。**重试保留拒绝时已观察到的最高版本，即使共享版本记录已被淘汰。** 其他错误和写操作不自动重试。预算不能简单相加：共享回源可能在调用者停止等待后才结束。

这些都是进程内任务数量限制，不是连接数、RSS 或集群数据库总配额。PostgreSQL 连接池上限跟随 `SOURCE_MAX_CONCURRENCY`；写入使用连接池及自己的超时，不占读 Getter 槽位。

### 缓存、TTL 与版本屏障

- main/hot LRU 使用 map、双向链表及独立互斥锁；命中会更新访问顺序。新条目复制 key，避免短键引用整个大请求字符串；value 入库也会复制。
- `CACHE_BYTES` 只统计 key 和 value 字节，不包含 Go 对象、索引、版本记录和请求内存。超大条目可直接返回，不为它清空其他条目。
- 读取时检查过期；后台每秒维护一次，每个区域每轮最多检查 128 条，尽力限制在 2 ms 内。锁忙则跳过，不刷新访问顺序或 TTL。逻辑删除引用不意味着 RSS 立即下降。
- hot 默认关闭；开启后从总预算预留空间，例如 64 MiB 总量中预留 8 MiB，main 剩 56 MiB。它保存可容纳的 Peer 结果，并非热点频率检测器。
- hot 寿命取所有者剩余 TTL、配置 hot TTL 与 1 秒的最小值，命中不续期。缺失/零 Peer TTL 可读取但不能进入 hot；非法 TTL 或版本属于协议错误。
- Bloom 仅从完整、不可变键集初始化。可写 PostgreSQL 模式关闭启动时 Bloom，避免新键被旧集合误拒绝。

每个 Group 有 256 个固定 epoch 分区。强制失效推进对应 epoch；版本通知仅在版本前进时推进。同一键的任务标识包含 epoch、最低版本与 key，通知处理和缓存回填共享锁，阻止旧任务在屏障之后重新填充缓存。哈希碰撞可能保守地阻止同分区其他键的回填，但不删除它们的已有值。

版本下界独立于缓存值，每组最多保留 **1,024 条**，按最后一次版本前进的顺序淘汰；读取、重复或更旧通知不延长保留时间。淘汰会再次推进 epoch，执行中的任务仍保留自身已观察到的最低版本，Peer 回退也不能丢失该约束。

版本元数据有界且易失。记录淘汰、节点重启或漏通知后，新请求不能依赖已遗忘的历史。它不能发现尚未观察到的数据库更新；不存在公开的最低版本读或线性一致读接口。

### 写入接口与一致性

键长度为 1–256 字节，值最多 1 MiB；写入值须为合法 JSON。PostgreSQL 读取返回与值同一快照的版本，API 使用强 ETag `"vN"` 表达它。写入及失效接口要求 Bearer token。

| 操作 | 条件与行为 |
| --- | --- |
| `GET /api?key=...` | 读取缓存/数据源，版本化数据返回 ETag |
| `PUT /api?key=...` 创建 | `If-None-Match: *`；已有有效记录则冲突 |
| `PUT /api?key=...` 替换 | `If-Match: "vN"`；使用读取到的确切版本 |
| `DELETE /api?key=...` | `If-Match: "vN"`；逻辑删除并保留版本 |
| `POST /invalidate?key=...` | 只失效缓存；可携带外部事务已提交版本 |
| Peer `DELETE /_geecache/products/...` | 鉴权后只失效本地副本，不再次广播 |

数据库在同一 SQL 中检查条件并增加版本。逻辑删除清空载荷但保留墓碑；重建继续增加版本，防止旧 ETag 匹配新记录。墓碑尚无自动回收；物理删除前必须设计保留代际的机制。JSON 内容中的业务版本不等于数据库版本列。

| 返回结果 | 含义 |
| --- | --- |
| `200`，`source_committed=true` | 数据库已提交，本次已知成员快照中的通知目标均确认；不代表所有读者同步 |
| `202`，`source_committed=true` | 数据库已提交，部分通知失败；不回滚，PostgreSQL Outbox 事件留待后台重试 |
| `412`，`source_committed=false` | 条件不匹配，没有修改或广播；重新读取并处理冲突 |
| `428` | 缺少写入前置条件 |
| `400` | 非法键/条件/版本；不支持弱 ETag、列表或通配 `If-Match` 等形式 |
| `503/504`，`source_committed=null` | 数据源出错或超时，提交结果可能未知；核实后再决定是否重试 |

写任务总预算 3 秒，其中数据库写入 800 ms、广播 1 秒；最多 8 个广播 worker。调用者断连不会取消提交后的通知，服务关闭仍能终止任务。数据库错误也可能意味着提交结果未知，因此仍尝试强制失效。

通知携带 `X-SimpleCache-Invalidation-Version: N`，N 必须是已提交 SQL 返回的正 int64。旧通知和重复通知不会删除同版本或更新的值，也不刷新其 TTL。Peer 回显通知版本只是处理确认，不是持久化水位；旧 Peer 缺少版本确认时标记为 `unversioned`，整体结果为部分投递。

外部写入必须遵守相同版本规则并主动通知。API 仅 `POST /invalidate` 接受客户端提供该版本头；PUT/DELETE 从数据库结果获取版本。不带版本头表示强制清除已知副本，但保留已经知道的版本下界。

**单次广播尽力投递，PostgreSQL Outbox 支持未完成事件的持久化重试；epoch 只保护本地回填。** 提交后崩溃、漏通知、离线成员和分布式在途读取仍可能暴露旧值；TTL 不是“提交后固定时间内最新”的保证。过期值不作为源故障时的隐式兜底，旧 ETag 也不能覆盖不同的当前数据库版本。

### 成员发现与 Operator

Peer 模式使用 CRC32、一致性哈希和每成员 100 个虚拟节点。节点身份为 `namespace/podName`，地址只是传输位置；新成员视图包含不可变哈希环、客户端和成员列表，通过原子指针发布，不迁移已有缓存。

每个缓存 Pod 监听所属命名空间和 Peer Service 的 EndpointSlice，选择有效 IPv4、命名 peer TCP 端口及未终止端点，排除 `ready=false`。首次同步并成功发布后即可 Ready，空集合也有效；监听失败保留旧视图并记录错误，成员视图最终收敛。

Operator 创建 StatefulSet、无头 Peer Service、API Service、ServiceAccount、发现 Role 和 RoleBinding；Gateway 模式再管理代理、路由、策略及每分片 Service。Reconcile 检查所有权、设置默认值并验证配置，幂等更新资源，监听子资源修复漂移。保留已分配地址、不可变身份、API 默认值和无关 Pod 字段，不接管同名无关资源。

Ready 检查 StatefulSet rollout；Gateway 模式还检查相关资源当前代际的接受/编程状态。它不证明全代理视图一致或数据库持续健康。配置、镜像或 Secret 引用变化可能滚动 Pod，Secret 内容变化本身需要受控重启。删除 CR 依赖 OwnerReference 和 Kubernetes GC；数据库由外部提供。

所有端口绑定后才开始服务；启动/存活探针为 `/healthz`，就绪探针为 `/readyz`。关闭时先标记未就绪，最多排空 HTTP 5 秒，再取消共享任务；Pod 宽限期 10 秒。

<a id="agreed-target-architecture"></a>

## 目标架构

主线是 **Gateway 按键路由 → 本地缓存 → 有界数据库回源；数据库条件写入 → 版本失效通知**。先采用每分片一个可丢弃缓存副本，再根据恢复实验决定是否增加副本或可靠通知。

实现固定使用 Envoy Gateway v1.9.2 / Gateway API v1.6.1，`BackendTrafficPolicy` 通过查询参数 `key` 使用 Maglev，表大小 65,537。每个分片 Service 只选择一个 StatefulSet 序号，Envoy 对稳定 Service IP 哈希；Pod 替换不改变该地址，Service 重建则会改变。多个加权 backendRef 不等同于这个单一哈希集群。

Gateway API 要求只含一个 `key` 参数，并使用规范 UTF-8 编码：百分号转义大写、空格为 `%20`。例如 `key=product%3A42` 合法，原始冒号、`%3a`、以 `+` 表示空格、重复 key 或额外参数均返回 400。Envoy 对线上的编码值哈希，这一约束避免同一业务键落到不同分片。GET、条件写入及失效接口均遵守该规则；Peer 模式保留原查询行为。

代理每 2 秒通过分片 Service 探测 `/readyz`，超时 1 秒。xDS 和健康视图异步收敛，拓扑变化期间可能重新映射、重复加载或失败；没有瞬时全局切换、数据迁移或热副本故障接管。需要键亲和的客户端应走 Envoy，`<name>-api` 仅作内部诊断/旁路入口。

后续能力按需求引入：

- **可靠通知**：已加入事务 Outbox/relay，重试未完成事件。一个队列对应一个缓存通知域，未发现/离线节点仍靠 TTL 或后续补齐；不是全局强一致读。
- **缓存副本**：冷恢复过慢、回源压力或单分片瓶颈有证据时再增加；先分片，再选择副本，定义版本、过期继承和重连补齐规则。
- **维护与扩容**：分批、有界地回收历史副本，不能逐键后台查询数据库；计算所有 Pod 的合计数据库压力。当前 `size` 是 Pod 数，不是分片/副本组合接口。

库存、订单、任务的业务约束应由业务服务的原子谓词、事务及幂等规则维护。Raft、缓存权威存储、Sidecar/DaemonSet 和 HPA 不属于第一阶段；未来自动扩缩容仍须保持单一副本数控制者。

<a id="run-and-configure"></a>

## 运行与配置

以下命令除明确 `cd` 外，均从仓库根目录执行。Go 版本要求见两个模块的 `go.mod`。

### 本地 demo

```bash
cd geecache-engine
go run -buildvcs=false ./cmd/simplecache -port=8001 -api=true
```

另一个终端读取：

```bash
curl -fsS 'http://localhost:9999/api?key=Auto-666'
```

demo 包含 Tom、Jack、Sam 和 Auto-0 至 Auto-9999，模拟回源延迟 100 ms。多节点静态部署设置 `SELF_ADDR` 和逗号分隔的 `PEERS` HTTP 地址。

### 主要配置

| 环境变量 | 默认值或要求 |
| --- | --- |
| `READ_ROUTING_MODE` | `peer`；`gateway` 直接读取且要求 hot 为 0 |
| `CACHE_BYTES` / `HOT_CACHE_BYTES` | 64 MiB / 0；总量范围 1–128 MiB，hot 小于总量 |
| `TTL_SECONDS` | 60，范围 1–3,600 |
| `SOURCE_MAX_CONCURRENCY` / `SOURCE_MAX_PENDING` | 32 / 128；pending 范围 0–4,096，0 禁用排队 |
| `MAX_MISS_WAITERS` | 128，范围 1–4,096，分别用于 API/Peer 等待及共享路由任务 |
| `WRITE_MAX_CONCURRENCY` | 16，范围 1–256 |
| `SOURCE_MODE` | `demo`；`postgres` 要求数据库地址和管理 token |
| `DATABASE_URL` / `CACHE_ADMIN_TOKEN` | 默认未设置；数据库连接串 / 共享 Bearer token |
| `API_ADDR` | `0.0.0.0:9999` |
| `DISCOVERY_MODE` | `static`；`kubernetes` 要求 `POD_NAME`、`POD_NAMESPACE`、`PEER_SERVICE` |
| `DEBUG_ADDR` | 默认关闭，只允许显式的 loopback IP 地址 |

### PostgreSQL 示例与条件写入

```bash
docker compose -f deploy/products/compose.yaml up -d --build
curl -fsS http://127.0.0.1:19999/readyz
curl -fsS 'http://127.0.0.1:19999/api?key=product:42'

ETAG=$(curl -fsS -D - -o /dev/null 'http://127.0.0.1:19999/api?key=product:42' \
  | awk 'tolower($1)=="etag:" {sub("\r$", "", $2); print $2}')
curl -i -X PUT 'http://127.0.0.1:19999/api?key=product:42' \
  -H "If-Match: $ETAG" \
  -H 'Authorization: Bearer learning-only-token' \
  -H 'Content-Type: application/json' \
  --data '{"id":42,"name":"Updated product","price_cents":12000}'
```

新建使用 `If-None-Match: *`；删除使用读取到的 `If-Match`。示例部署三个缓存，每个逻辑预算 16 MiB、容器内存上限 256 MiB，数据库使用持久卷，默认初始化 1,000 条小记录。可通过 `CACHE_ADMIN_TOKEN` 覆盖示例 token，所有节点和客户端保持一致；对外暴露写接口时使用可信网络和 TLS 终止。

已有数据库卷不会重新初始化。升级前停旧写入实例、显式执行 schema 迁移，再启动新版：

```bash
docker compose -f deploy/products/compose.yaml stop cache-1 cache-2 cache-3
docker compose -f deploy/products/compose.yaml up -d --wait postgres
docker compose -f deploy/products/compose.yaml exec -T postgres \
  psql -U simplecache -d simplecache -v ON_ERROR_STOP=1 < deploy/products/schema.sql
docker compose -f deploy/products/compose.yaml up -d --build
```

迁移可重复执行并保留载荷；不要混用忽略版本列的旧写入者。seed 不覆盖已修改或逻辑删除的记录。`docker compose down` 保留数据库卷，额外使用 `--volumes` 才会删除数据。

通知部分失败时，只重试通知，不重复业务写入：

```bash
curl -i -X POST 'http://127.0.0.1:19999/invalidate?key=product:42' \
  -H 'Authorization: Bearer learning-only-token'
```

外部已提交写入可额外携带 `X-SimpleCache-Invalidation-Version`，取值必须来自该事务的 SQL 返回版本。

### Kubernetes 与 Gateway

先单独准备数据库/schema，以及同命名空间、包含 `DATABASE_URL` 和 `CACHE_ADMIN_TOKEN` 的 Secret。最小 CR 示例：

```yaml
apiVersion: cache.x1kun.com/v1
kind: SimpleCache
metadata:
  name: products
spec:
  size: 3
  image: your-built-simplecache-image
  sourceSecret: product-source
  cacheBytes: 67108864
  hotCacheBytes: 0
  ttlSeconds: 60
```

业务 key 统一为 1–256 字节合法 UTF-8，不允许 NUL；不符合契约的请求返回 400。SimpleCache 名称需以字母开头，使用小写 DNS label，Peer 模式最多 59 字符，Gateway 模式最多 55 字符。

CR 还支持 `maxMissWaiters`、`sourceMaxPending`、`maxConcurrentWrites` 和资源 requests/limits。`size` 范围 1–10；默认 CPU request 100m、内存 request 128 MiB、limit 256 MiB。移除 `sourceSecret` 回到 demo 模式。

Gateway 部署需先安装固定版本 CRD，开启 Backend 扩展，准备 GatewayClass，并启用 Operator 的 `--enable-gateway`；随后为 CR 增加 `gateway: {className: eg, replicas: 2}`。代理副本范围 1–8。详细步骤见 [英文部署说明](design.md#envoy-gateway-deployment-and-smoke)。

隔离的传统 Kind 部署验证：

```bash
make kind-up
make kind-smoke
make kind-delete
```

该流程使用专用 `simplecache-stage4` 集群和 kubeconfig。Gateway 自动实验另行创建唯一名称的 Kind 集群；不使用已有无关集群，结束后尝试清理本轮集群及临时代理标签，清理失败会使运行失败。

<a id="validation-and-evidence"></a>

## 验证与证据

### 常用检查

```bash
make check                    # 格式、单测、Race、编译和 vet
make test-operator-ci         # 临时 API Server/etcd、Race 和控制器覆盖率
make generated-check k8s-render compose-config
make validate                 # 引擎 Race、对抗场景和诊断 profile
make postgres-compare COMPARE_QUICK=1
```

Operator envtest 验证默认值、所有权、状态与 watch 修复，不运行完整调度/网络/工作负载控制器。真实 PostgreSQL 集成测试需设置专用测试库的 `SIMPLECACHE_TEST_DATABASE_URL`；未设置时跳过，不能视为通过。初始化 schema 后，从引擎模块执行：

```bash
cd geecache-engine
SIMPLECACHE_TEST_DATABASE_URL='postgres://.../dedicated_test_db?sslmode=disable' \
  go test -race -count=1 \
  -run '^TestPostgres(OpenChecksExecutionErrors|SchemaMigration|ConditionalWrites|ProductLifecycle|VersionedInvalidation|ResponseLossRetry|CommitBeforeBroadcastCrash|Outbox.*)$' \
  -v ./internal/source/postgres ./internal/app
```

`postgres-compare` 是独立进程的数据库对照工具，默认 45 个试次，quick 为 15 个功能试次；它不是部署后的 Gateway 链路，结论需分开解释。

新增契约测试分别验证：A 写入成功但响应丢失后，原 If-Match 重试返回 412、SQL 版本只前进一次；B 已生成的旧响应晚到时，无保护客户端会倒退，按 key 保护版本的参考客户端不倒退，服务端缓存也不回退；C 写入子进程在提交后、广播前被 SIGKILL，存活缓存通过原 TTL 过期回源恢复。B 仅为客户端策略示例，A 不提供幂等结果重放，C 不期待自动补发。详见 [契约测试说明](../geecache-engine/internal/README.md#retry-response-delivery-and-notification-crash-contracts)。

### 真实集群实验

需要 Linux amd64/arm64、可用 Docker、kubectl、Python 3、curl、sha256sum 和 Go 构建工具。脚本下载并校验固定版本 Kind/Gateway，构建本地生产 Dockerfile，不发布镜像。

```bash
make gateway-smoke            # 基础功能：真实 Envoy、PostgreSQL、Operator
make gateway-experiments      # 基础功能 + 扩展故障恢复，共 26 项检查
make gateway-perf             # 六场景、两种路由、默认各重复三次

# 缩小规模，先验证完整链路
GATEWAY_PERF_REPEATS=1 GATEWAY_PERF_READS=4096 make gateway-perf

# 默认 Go 模块代理不可达时，可给上述命令增加此前缀
GOPROXY=https://goproxy.cn,direct make gateway-smoke
```

扩展实验覆盖多代理路由、条件写入竞争、无效请求、部分失效、1,030 键版本元数据压力、漏通知、TTL 恢复、数据库断连、Pod/代理替换及 Operator 子资源修复。

性能实验通过集群内客户端访问共享 Envoy Service，对照 RoundRobin 和按键哈希。场景为热键、冷键突发、容量压力、Zipf、95/5 混合读写和 TTL 周期。每个缓存预算 1 MiB，默认 512 个 4 KiB 值；测量前验证实际哈希落点能容纳其分片工作集。重置、预热及指标抓取不计入客户端测量时间。

客户端检查完整载荷、ETag 和版本。混合流量按每次读取发起时已确认的写入版本设置下界，低于下界同时记为 `stale_at_start` 和 `incorrect`，使试次失败。写入结果未知时不盲目重试。

默认清理实验资源。失败时需要保留现场，可设置 `GATEWAY_KEEP_FAILED=1`。符合条件的性能续跑使用：

```bash
GATEWAY_PERF_CONTINUE_REPORT=bin/reports/<retained-run> make gateway-perf
```

续跑会验证现场所有权、实例身份、预算及客户端校验和，并保留先前失败。错误数据、缺失请求、空试次、写错误或无法分类的错误使最终状态为 `failed`；仅明确的 GET 错误可标为 `completed-with-read-errors`。**续跑复用原客户端二进制；验证修改后的客户端应新建实验。** 任意准备阶段失败不一定满足续跑条件，见 [完整续跑约束](design.md#real-chain-routing-measurements)。

### 已有证据及适用范围

2026-10-29 的完整 Gateway 对照完成 36 个试次、607,410 次请求、768 次写入；完成试次中有 4 个 GET 503，另一次中断尝试保留 1 个错误。因此其结论为 `completed-with-read-errors`，不属于零错误基线；也不能代表随后加入重试的版本。

2026-10-30 补充验证使用修复后的代码：扩展实验 **26 项通过**；性能实验 **12 个试次、30,438 次请求、256 次写入**，错误、错误数据、陈旧读取和缺失请求均为 0。混合 RoundRobin 流量记录了 4 次旧结果拒绝和 4 次重试决策，最终没有对外错误。两个实验串行执行，集群和临时代理标签均通过 Docker 清理核验。

| 场景 | RoundRobin 回源次数 | 按键哈希回源次数 |
| --- | ---: | ---: |
| 热键 | 0 | 0 |
| 冷键突发 | 3 | 1 |
| 容量压力 | 2,756 | 0 |
| Zipf | 522 | 0 |
| 混合读写 | 674 | 127 |
| TTL 周期 | 12 | 4 |

以上为 2026-10-30 **单轮**数据。它支持按键路由减少重复缓存和回源的特定结论，不能推出生产吞吐上限或普遍延迟收益。宿主机有明显 I/O 干扰，实验控制面 etcd 和 PostgreSQL 数据使用内存存储；不验证物理磁盘卸载、数据库崩溃恢复或生产耐久性。

证据保存在被 Git 忽略的 `bin/reports/`，链接依赖本地文件保留：

- [扩展实验结果](../bin/reports/20261030T034953Z-gateway-ef206f/result.json)
- [性能与独立复核报告](../bin/reports/20261030T042855Z-gateway-34190a/verification.md)
- [Docker 清理核验](../bin/reports/20261030T042855Z-gateway-34190a/cleanup-verification.json)
- 更早的 Peer/hot、数据库及 Gateway 实验证据见 [英文历史记录](design.md#observed-results-and-their-limits)。

Getter 次数会同时受缓存和 SingleFlight 影响，不能直接当命中率；进程 RSS/heap 快照和 Kubelet 采样不是连续内存峰值。数据库/OS 缓存未清空，不能用独立百分位相减估算 Gateway 自身开销。报告保留原始请求、指标、二进制校验和及实例身份，失败与跳过均需显式记录。

### 事务 Outbox

适配器的条件写入及可信 Put/Delete 将数据变更与 `(key, 提交版本)` 事件放在同一 SQL 事务：412 不生成事件，事件插入失败则数据一起回滚。原同步通知和 HTTP 200/202 契约保留，后台可能重复发送已同步成功的版本。

每个有通知凭据的应用进程启动一个 worker；通过 SKIP LOCKED 领取事件并递增租约令牌，在数据库事务外发送，只允许未过期的当前令牌确认。默认空闲轮询 1 秒、租约 10 秒、发送预算 2 秒、数据库操作预算 1 秒，失败退避从 1 秒增加到最多 60 秒。未知确认结果或 worker 崩溃后，租约过期即可接管；不按失败次数丢事件。

**同一 search_path 中的队列只服务一个缓存通知域。** 发送完成针对一个非空的已知成员快照，未知/离线副本仍依赖 TTL；完成事件不会因新节点加入自动重放。原始外部 SQL 不自动生成事件，必须另行遵守版本和事件事务契约。

升级仍执行 `deploy/products/schema.sql`，先停旧写入者。现有卷不会自动初始化新表；启动会检查 Outbox 列。worker 与读写共用连接池，在 HTTP 排空后取消并等待退出，再关闭 Router/Store。每 30 秒采样积压，以每批 1,000 条、总共 1 秒的时间预算追赶清理已完成且超过 7 天的事件；未完成事件不清理，7 天不是严格删除时限。

新验证包含：事件失败导致源回滚、并发独占领取、租约过期接管与旧令牌隔离、写入进程 SIGKILL 后新应用补发，以及发送后确认丢失的重复投递。旧 C 测试表示发送器尚未运行时的 TTL 兜底，新 C 测试验证发送器恢复后在 TTL 前修复缓存。B 的客户端保护仍仅为测试参考，不增加 UI、幂等结果存储。

指标为 `simplecache_outbox_operations_total{operation,result}`、`simplecache_outbox_pending_events` 和 `simplecache_outbox_oldest_pending_seconds`。积压是各进程对同一队列的采样，不能跨 Pod 求和；Getter 次数不包含轮询 SQL。详见 [Outbox 设计](../geecache-engine/internal/outbox/README.md)。

监控 `simplecache_outbox_purge_eligible_events`（超期已投递事件数）与 `simplecache_outbox_table_bytes`（含索引的表空间），均为共享队列的采样视图，不跨实例求和。

PostgreSQL 对照压测的 `postgres-fixture` 未启动 Outbox relay；写入仍持久化事件，但测量不包含 relay 轮询、重放广播、维护 SQL 及其连接池竞争。

### 观测与 CI

`/metrics` 区分 API/Peer、回源、排队、写入、广播、版本拒绝、缓存与主动清理。重点指标包括：

- `simplecache_source_retries_total{reason="stale_version"}`：重试决策，包含从已刷新缓存恢复。
- `simplecache_version_rejections_total`：版本保护拒绝，`kind` 标签为 `source` 或 `peer`。
- `simplecache_version_records` / `simplecache_version_record_evictions_total`：版本记录占用与淘汰。
- `simplecache_source_write_duration_seconds` / `simplecache_invalidation_broadcast_duration_seconds`：数据库写入与通知广播分段耗时。
- `simplecache_source_unknown_commits_total`：提交结果未知；412 冲突不计入。

指标标签不包含键、版本、任意 URL 或错误文本。诊断 profile 通过独立 loopback `DEBUG_ADDR` 显式开启；使用同次运行保留的二进制分析 CPU/heap/alloc/mutex/block，带 profile 的实验与无采样基线分开。

CI 在 PR、main 推送或手动触发时执行格式、vet、Race、Python 回归、编译、Compose、生成物、Operator envtest 和 PostgreSQL 集成/快速对照。容器工作流只构建不发布；Kind 和重型实验需单独执行。

<a id="delivery-plan"></a>

## 后续计划

| 状态 | 工作与验收方向 |
| --- | --- |
| 已实现 | Gateway 直接路由、条件写入、版本失效、有界元数据、主动过期及并发保护 |
| 已补充验证 | 旧版本有界重试、版本记录淘汰保护、LRU key 所有权、响应写超时、续跑失败汇总 |
| 下一步 | 在受控资源环境建立容量、过载恢复和长时间混合负载基线，继续核对容器内存及集群数据库总预算 |
| 本轮加入 | 事务 Outbox、租约接管、持久化重试与崩溃恢复验证 |
| 按需引入 | 离线节点补齐或缓存副本，按实际需求独立决策 |

完整参数、协议边界、历史证据与参考链接见 [英文版](design.md)。
