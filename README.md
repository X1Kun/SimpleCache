# SimpleCache - 云原生分布式高可用缓存系统

基于 Go 设计并实现的去中心化云原生分布式缓存系统。项目深度整合了分布式存储原理与 Kubernetes 云原生架构，旨在解决高并发场景下的数据访问瓶颈、缓存穿透与击穿问题，并实现集群的自动化运维与监控。

## 🚀 核心特性

### 1. 去中心化缓存引擎
* **LRU 淘汰算法**：底层采用 LRU（最近最少使用）缓存策略，配合细粒度读写锁，保障单机高并发读写安全。
* **一致性哈希路由**：实现带虚拟节点的一致性哈希算法，构建动态路由环。解决节点动态扩缩容时的数据倾斜问题，实现平滑迁移。
* **高效内部通信**：节点间通信采用自定义 HTTP 接口，并引入 Protobuf 进行二进制序列化，大幅降低集群内部 (Peer Fetch) 的网络传输开销。

### 2. 高并发容灾防御体系
* **防击穿 (SingleFlight)**：在后端回源链路深度集成 SingleFlight 请求合并机制。面对极热点 Key 的海量并发请求，严格将回源动作收敛至 **1 次 DB 查询**，有效保护底层数据源。
* **防穿透 (Bloom Filter)**：引入布隆过滤器前置拦截非法请求。在 10W 级并发恶意随机 Key 压测中，精准拦截 99.99% 无效访问，拒绝无效穿透。

### 3. 云原生编排与可观测性
* **K8s Operator 自动化运维**：基于 Kubebuilder 编写自定义控制器，将缓存集群抽象为 `SimpleCache` CRD。深度接管 StatefulSet 与 Headless Service，实现节点的自动化部署、网络拓扑生成与故障自愈。
* **全链路监控大屏**：代码级深度集成 Prometheus 埋点，上报节点 Hit、PeerFetch、SlowDBFetch 等核心吞吐指标。配合 Grafana 构建毫秒级端到端可视化监控大屏。

---

## 📂 目录结构

```text
.
├── geecache-engine/       # 核心分布式缓存引擎源码 (Go)
│   ├── consistenthash/    # 一致性哈希算法实现
│   ├── lru/               # LRU 缓存淘汰算法
│   ├── singleflight/      # 防击穿请求合并逻辑
│   ├── bloomfilter.go     # 防穿透布隆过滤器逻辑
│   └── geecachepb/        # Protobuf 协议定义文件
├── simplecache-operator/  # K8s 自定义控制器源码 (Kubebuilder)
│   ├── api/               # CRD API 结构定义
│   └── internal/          # Reconcile 调谐逻辑实现
└── values.yaml            #  Helm / K8s 部署配置参数
📊 性能压测报告 (Benchmark)
在本地单机 Kubernetes (Kind) 环境下，针对 5 节点集群进行极限压测：

吞吐量 (QPS)：单节点稳定吞吐量达 5000+ QPS。

防穿透极限测试：发起 100,000 次随机并发请求，Bloom Filter 成功拦截 99,997 次无效请求，拦截率达 99.99%。

防击穿极限测试：10,000 并发死磕单一热点 Key，回源 DB 查询次数稳定控制为 1 次，其余请求全部复用合并结果。

💡 附：监控大屏截图
(请在这里贴上你那张完美的 Grafana Hit/PeerFetch 监控图)

🛠️ 快速开始
1. 本地运行控制器
Bash
cd simplecache-operator
make install
make run
2. 部署缓存集群
Bash
# 应用 CRD 实例与相关配置
kubectl apply -f config/samples/xxx.yaml
3. 执行基准测试
Bash
# 暴露 Service 端口
kubectl port-forward svc/my-first-geecache-svc 9999:9999

# 运行高并发压测脚本
cd geecache-engine
go run benchmark_test.go
```

<img width="1560" height="298" alt="image" src="https://github.com/user-attachments/assets/7db5bd61-849f-4bfa-8966-c2819e175692" />
<img width="1538" height="306" alt="image" src="https://github.com/user-attachments/assets/8e6a822c-ad24-447f-bf94-4190e92b239e" />
<img width="1536" height="316" alt="image" src="https://github.com/user-attachments/assets/95853152-11a7-4cf9-9f1d-0e9560ebdf8f" />

