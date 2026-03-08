package geecache

import (
	"fmt"
	pb "geecache/geecachepb"
	"geecache/singleflight"
	"log"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus指标定义
var (
	// 记录缓存请求总数的计数器，带有一个 label 叫 "status"
	cacheRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "geecache_requests_total",
			Help: "Total number of cache requests",
		},
		[]string{"status"}, // 状态标签：hit (命中), peer_fetch (远端获取), slowdb_fetch (穿透到底层)
	)
)

// 回调接口
type Getter interface {
	Get(key string) ([]byte, error)
}

// 给这样结构的函数起别名（函数结构体）
type GetterFunc func(key string) ([]byte, error)

// 给函数结构体实现Get接口，这样利用GetterFunc创建的匿名结构体可以直接当做回调接口使用
func (f GetterFunc) Get(key string) ([]byte, error) {
	return f(key)
}

// 缓存组，可以按name储存不同类型的数据，并设置不同的回调函数
type Group struct {
	name       string // 解释组的含义 + 帮groups寻找map项
	getter     Getter
	mainCache  cache
	peers      PeerPicker // 每个组都存在一个peers，这个peers每个组应该都是一样的吧
	loader     *singleflight.Group
	bloom      *BloomFilter  // 布隆过滤器
	defaultTTL time.Duration // 整个 Group 的默认缓存存活时间
}

var (
	// 缓存组的读写锁，只有对组的操作才需要加锁，锁内部的cache不用它管
	mu     sync.RWMutex
	groups = make(map[string]*Group)
)

// 创建组 1、检查回调函数 2、加锁 3、利用传入参数创建组，并写入map中
func NewGroup(name string, cacheBytes int64, getter Getter) *Group {
	if getter == nil {
		panic("nil Getter")
	}
	mu.Lock()
	defer mu.Unlock()
	gp := &Group{
		name:       name,
		getter:     getter,
		mainCache:  cache{cacheBytes: cacheBytes},
		loader:     &singleflight.Group{},
		bloom:      NewBloomFilter(100000, 3),
		defaultTTL: time.Minute * 5,
	}
	gp.bloom.Add("Tom")
	gp.bloom.Add("Jack")
	gp.bloom.Add("Sam")

	groups[name] = gp
	return gp
}

// 获得组 1、加读锁 2、返回groups根据name找到的组
func GetGroup(name string) *Group {
	mu.RLock()
	defer mu.RUnlock()
	gp := groups[name]
	return gp
}

// 组中找元素
// 1、如果key为空，返回空 2、如果在组内的cache中找到了元素，则返回 3、否则，执行load进行回源
func (g *Group) Get(key string) (value ByteView, err error) {
	if key == "" {
		return ByteView{}, fmt.Errorf("key is required")
	}
	if v, ok := g.mainCache.get(key); ok {
		// log.Println("[GeeCache] hit")
		// 本地缓存命中
		cacheRequestsTotal.WithLabelValues("hit").Inc()
		return v, nil
	}
	return g.load(key)
}

// 回调
// 通过本地/哈希环获取数据
func (g *Group) load(key string) (value ByteView, err error) {

	// 如果布隆过滤器说不存在，直接返回错误
	if !g.bloom.Contains(key) {
		return ByteView{}, fmt.Errorf("布隆过滤器拦截: 恶意或不存在的 Key [%s]", key)
	}

	// singleflight
	viewi, err := g.loader.Do(key, func() (interface{}, error) {
		// 远程获取（分布式系统）
		if g.peers != nil {
			if peer, ok := g.peers.PickPeer(key); ok {
				if value, err := g.getFromPeer(peer, key); err == nil {
					// 从其他节点拿到数据
					cacheRequestsTotal.WithLabelValues("peer_fetch").Inc()
					return value, nil
				}
				log.Println("[GeeCache] Fail to get from peer", err)
			}
		}
		// 本地回源
		cacheRequestsTotal.WithLabelValues("slowdb_fetch").Inc()
		return g.getLocally(key)
	})
	if err == nil {
		return viewi.(ByteView), nil
	}
	return
}

// 本地回源
// 1、利用组中的getter方法进行回源 2、将回源得到的字节切片深拷贝，放入cache
func (g *Group) getLocally(key string) (ByteView, error) {
	bytes, err := g.getter.Get(key)
	if err != nil {
		return ByteView{}, err
	}
	value := ByteView{b: cloneBytes(bytes)}
	g.populateCache(key, value)
	return value, nil
}

// cache的详细处理细节
// 1、cache中添加元素 2、（可选）cache hit计数，日志记录，数据处理
func (g *Group) populateCache(key string, value ByteView) {
	g.mainCache.add(key, value, g.defaultTTL)
}

// 为缓存组添加peers
func (g *Group) RegisterPeers(peers PeerPicker) {
	if g.peers != nil {
		panic("RegisterPeerPicker called more than once")
	}
	g.peers = peers
}

// 从peer中根据数据标签获取数据
func (g *Group) getFromPeer(peer PeerGetter, key string) (ByteView, error) {

	req := &pb.Request{
		Group: g.name,
		Key:   key,
	}
	res := &pb.Response{}
	err := peer.Get(req, res)
	if err != nil {
		return ByteView{}, err
	}
	return ByteView{b: res.Value}, nil
}
