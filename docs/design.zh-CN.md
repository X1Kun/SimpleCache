# SimpleCache 设计与运行

SimpleCache 是 PostgreSQL 支撑的加载式缓存。每个实例只读取本地缓存和数据库；
多实例按 key 分片由 Envoy Gateway 完成。实例间仅交换失效通知，不传输缓存值。
无 Gateway 的多个实例是独立本地缓存副本，可能产生重复缓存和回源。

## 结构与读取

客户端 → Gateway 按 key 选择分片 → 本地 LRU/TTL → 未命中时回源 PostgreSQL。
单实例可以直接访问 API，无需 Gateway。Operator 管理 StatefulSet、Service 及可选
Gateway 资源；分片 Service 地址稳定，Pod 替换不需要以 Pod IP 作为分片身份。
EndpointSlice 仅用于发现通知目标，不决定读取归属。

`cache.Group.Get` 是唯一读取入口。命中不占用未命中等待额度；同 key/版本屏障的
未命中共享一层 source singleflight。执行中的回源和排队回源分别有界，容量不足
返回 503 / Retry-After。客户端取消只停止等待，共享回源保留自己的生命周期和截止
时间。旧版本回源最多重试一次，不延长原截止时间，不丢失已观察到的版本下限。

`CACHE_BYTES` 仅计 key+value 字节，不包含对象、索引、连接、在途请求和进程 RSS。
大于缓存预算的合法值可返回，但不会插入缓存或清空其他条目。value 最大 1 MiB；
key 必须是 1–256 字节合法 UTF-8，不含 NUL。TTL 不因命中续期。过期通过读取和
有界后台游标清理；不会在数据库故障时隐式返回已过期数据。

版本校验与回填在同一临界区完成，锁序为 `epochMu -> cache.mu`。256 个 epoch
条带与最多 1024 条版本下限元数据约束晚到回填。淘汰版本记录也会推进相应 epoch；
在途任务仍保留自己的最低版本。失效会阻止旧值回填，但不是全局读取同步屏障。

## 读写 API 与一致性

| 请求 | 契约 |
| --- | --- |
| `GET /api?key=...` | 返回值及版本 ETag `"vN"` |
| `PUT /api?key=...` | 鉴权、合法 JSON；替换需精确 If-Match，创建需 If-None-Match: * |
| `DELETE /api?key=...` | 鉴权并携带精确当前 If-Match |
| `POST /invalidate?key=...` | 鉴权，仅清缓存；可携带正整数提交版本，未携带则强制清理 |
| `/healthz`、`/readyz`、`/metrics` | 健康、就绪与指标 |

所有 API 使用同一规范化 key：唯一 `key` 参数、UTF-8、大写百分号编码、空格用
`%20`。例如 `product:42` 写为 `key=product%3A42`。重复参数、额外参数、替代编码
和非法字符在回源前拒绝。

数据库原子检查版本、修改数据并写入 Outbox 事件。412 表示条件失败、没有提交，
不广播。删除保留墓碑版本，重新创建继续递增，避免旧 ETag 再次匹配。外部 SQL
写入者必须自行遵守数据、版本、事件的原子事务契约；没有自动 trigger/CDC。

提交成功返回 `source_committed=true`、提交版本和已知成员快照的通知报告。200 表示
该快照全部确认；202 表示已提交但部分通知未确认，不回滚。源故障可能是未知提交，
返回 `source_committed=null`，不能当作确定未写入。已开始的写协调不因客户端断开而
取消；写入额度覆盖 body 缓冲、数据库、通知和响应交付。

本服务提供弱一致读：在途响应、漏通知、未知/离线副本可能暴露旧值。通知确认不是
线性一致屏障；TTL 是每次填充的寿命，不是提交后固定时间内最新的承诺。条件写
防止旧 ETag 覆盖不同的当前源版本。响应丢失后用旧条件重试返回 412，不存储幂等
结果。测试中的版本保护客户端只是参考策略，不是已交付的 SDK。

## 通知与 Outbox

`cluster.Directory` 发布不可变的成员/HTTP 客户端快照，只负责通知。8001 内部端口
仅接受带 token 的 `DELETE /_simplecache/invalidate/<group>/<escaped-key>`，返回所
接受的版本确认。GET 不能读取数据。通知最多 8 个并行任务，总预算 1 秒。重复、
乱序和旧删除通知不会清掉更新的缓存；强制失效也保留已知版本下限。

同一 PostgreSQL search_path 的 Outbox 只服务一个缓存通知域，不能给独立缓存部署
当竞争消费者共享。有凭据的实例启动一个 worker，以 SKIP LOCKED 领取并递增租约
令牌，事务外发送，仅允许未过期的当前令牌确认。丢失领取/确认结果或实例崩溃后由
租约过期接管。非空的已知成员快照全部确认后完成；未知/离线副本仍靠 TTL 或恢复。

默认租约 10 秒、投递预算 2 秒、存储操作 1 秒、空闲轮询 1 秒；失败退避 1–60 秒。
每 30 秒采样积压和存储，以每批 1000 条、共享 1 秒预算清理超过 7 天的已投递事件；
未完成事件不清理。7 天是删除资格，不是表大小或严格删除期限保证。

关注 pending/最老事件年龄、超期已投递数量和含索引的表空间。它们是对同一队列的
进程采样，不跨 Pod 求和。worker 与读写共用连接池；Getter 次数不包含后台 SQL。
详见 [Outbox 契约](../geecache-engine/internal/outbox/README.md)。

<a id="run-and-configure"></a>

## 运行与配置

单实例：

```bash
cd geecache-engine
go run -buildvcs=false ./cmd/simplecache -port=8001 -api=true
curl -fsS 'http://localhost:9999/api?key=Auto-666'
```

PostgreSQL 示例提供三个独立本地缓存及通知广播：

```bash
docker compose -f deploy/products/compose.yaml up -d --build
curl -fsS 'http://127.0.0.1:19999/api?key=product%3A42'
```

多实例按键分片使用 `spec.gateway`、控制器 `--enable-gateway` 和固定版本 Envoy
Gateway。RoundRobin/按键哈希对照共用 Gateway 与同一本地缓存读取实现。

| 配置 | 默认与含义 |
| --- | --- |
| SOURCE_MODE | demo；postgres 需 DATABASE_URL 和 CACHE_ADMIN_TOKEN |
| API_ADDR | 0.0.0.0:9999；CLI 默认开启 API |
| -port | 8001，通知端口；-api=false 只关闭业务 API |
| CACHE_BYTES | 64 MiB，范围 1–128 MiB |
| TTL_SECONDS | 60，整数秒，范围 1–3600 |
| SOURCE_MAX_CONCURRENCY | 32，范围 1–1024 |
| SOURCE_MAX_PENDING | 128；显式 0 禁用排队 |
| MAX_MISS_WAITERS | 128，范围 1–4096 |
| WRITE_MAX_CONCURRENCY | 16，范围 1–256 |
| DISCOVERY_MODE | static 或 kubernetes |
| SELF_ADDR / MEMBERS | 静态通知地址；MEMBERS 默认仅 self |
| POD_NAME / POD_NAMESPACE / CLUSTER_SERVICE | Kubernetes 身份和通知用 headless Service |
| DEBUG_ADDR | 显式 loopback 诊断端口，不使用公开 DefaultServeMux |

API 读取等待 1500ms，共享源任务默认 800ms。写协调 3 秒，其中源写入独立预算
800ms。数据库启动检查 5 秒；HTTP header/body/idle/write 预算分别为 3/5/30/10 秒，
排空 5 秒。监听端口全部绑定成功才启动发现和声明就绪；先排空 HTTP，再停止并等待
relay，最后关闭目录和源。

CR 支持 size(1–10)、image、cacheBytes、TTL、sourceSecret、未命中/排队/写入额度、
resources 与可选 gateway。名称是字母开头的小写 DNS label，无 Gateway 最多 59
字符、有 Gateway 最多 55 字符。默认 CPU request 100m、memory request 128 MiB、
memory limit 256 MiB。Secret 保持同 namespace 引用，控制器不读取凭据内容。

## 从已退役读取模式迁移

这是不兼容的配置与内部协议收敛。暂停写入，统一升级引擎、Operator/CRD 与部署
配置；不支持混用旧/新内部通知协议。业务数据、版本与 Outbox SQL 表结构不变。
保留现有 `-svc` Service 身份，EndpointSlice 通知端口名改为 `cluster`。

| 移除项 | 处理方式 |
| --- | --- |
| READ_ROUTING_MODE（包括 gateway） | 删除；实例统一本地读取 |
| HOT_CACHE_BYTES / CR hotCacheBytes | 删除远端副本预算；普通热点仍由本地 LRU 承接 |
| PEERS / PEER_SERVICE | 改为 MEMBERS / CLUSTER_SERVICE，只配置通知目标 |
| 实例间 GET / Protobuf / 自建哈希环 | 由 Gateway 路由并由当前实例本地加载 |
| 原内部失效 URI | 改为 /_simplecache/invalidate/...，版本确认 header 保留 |
| postgres-compare 及独立装配运行时 | 使用 Gateway 对照和正式运行时 arrival 验证 |

非空旧环境变量启动即报错并提示迁移。应用新 CRD，从 CR 清单移除旧字段；检查
readyz、已知 key 读取、条件写入和所有发现成员的通知确认后恢复正常写入。

<a id="validation-and-evidence"></a>

## 验证与下一步

```bash
make check
make test-operator-ci
make generated-check k8s-render compose-config
make validate
make gateway-smoke
make gateway-experiments
make gateway-perf
make gateway-arrival
make arrival-calibration
```

真实 PostgreSQL 测试需要专用已初始化的 SIMPLECACHE_TEST_DATABASE_URL；未设置会
明确跳过，不能当作数据库验证通过。测试保留条件竞争、删除重建、旧回填、版本
记录淘汰、取消、响应丢失、提交后 SIGKILL、Outbox 重放和租约隔离。envtest 不运行
完整工作负载/网络控制器；Kind 使用唯一集群、专用 kubeconfig、保留证据并清理本轮
资源，同时明确检查通知端口拒绝 GET。

[Arrival 实验](experiments/arrival.md) 与 [Kind 实验](experiments/arrival-kind.md) 说明
调度、用户群、资源与当前测量边界。旧报告仍保留在 Git 忽略的 bin/reports/，绑定
原二进制与环境；旧 Peer 结果属于历史证据，不代表当前仍支持该模式。本次重构
不增加生产容量结论。下一步仍是大 value、工作集扫描、冷热点、慢源和长期过载恢复。
