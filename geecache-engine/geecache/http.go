package geecache

import (
	"fmt"
	"geecache/consistenthash"
	pb "geecache/geecachepb"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
)

const (
	defaultBasePath = "/_geecache/"
	defaultReplicas = 50
)

type HTTPPool struct {
	self        string
	basePath    string
	mu          sync.Mutex             // 作用是什么？
	peers       *consistenthash.Map    // 一致性哈希环
	httpGetters map[string]*httpGetter // 格式为"http://10.0.0.2:8008"
}

// 一个Getter相当于一个客户端
type httpGetter struct {
	baseURL string
}

func NewHTTPPool(self string) *HTTPPool {
	return &HTTPPool{
		self:     self,
		basePath: defaultBasePath,
	}
}

func (p *HTTPPool) Log(format string, v ...interface{}) {
	log.Printf("[Server %s] %s", p.self, fmt.Sprintf(format, v...))
}

// 根据收到的http请求，去缓存组获取数据
func (p *HTTPPool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, p.basePath) {
		panic("HTTPPool serving unexpected path: " + r.URL.Path)
	}
	p.Log("%s %s", r.Method, r.URL.Path)
	parts := strings.SplitN(r.URL.Path[len(p.basePath):], "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	groupName := parts[0]
	key := parts[1]
	group := GetGroup(groupName)
	if group == nil {
		http.Error(w, "no such group"+groupName, http.StatusNotFound)
		return
	}
	view, err := group.Get(key)
	body, err := proto.Marshal(&pb.Response{Value: view.ByteSlice()})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-type", "application/octet-stream")
	w.Write(body)
}

// 为http连接池配置哈希环 1.加锁 2.初始化哈希环 3.物理桶打散成虚拟桶，放在环上 4.配置物理桶->url的映射
func (p *HTTPPool) Set(peers ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peers = consistenthash.New(defaultReplicas, nil)
	p.peers.Add(peers...)
	p.httpGetters = make(map[string]*httpGetter, len(peers))
	for _, peer := range peers {
		// 将会变成"http://10.0.0.1:8001/_geecache/"
		p.httpGetters[peer] = &httpGetter{baseURL: peer + p.basePath}
	}
}

// 根据数据标签，找到真实的url 1.加锁 2.数据标签->物理桶（哈希一致性） 3.物理桶->存放url的结构体
func (p *HTTPPool) PickPeer(key string) (PeerGetter, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 如果peer的名字是自己，那么数据就要从本地获得
	if peer := p.peers.Get(key); peer != "" && peer != p.self {
		p.Log("Pick peer %s", peer)
		return p.httpGetters[peer], true
	}
	return nil, false
}

// 防呆设计，防止写漏了接口方法PickPeer
var _ PeerPicker = (*HTTPPool)(nil)

// 发送http消息，给其他的缓存节点
func (h *httpGetter) Get(in *pb.Request, out *pb.Response) error {
	u := fmt.Sprintf(
		"%v%v/%v",
		h.baseURL,
		// url编码：正确识别原始字符串
		// "My Group" -> "My%20Group", "a/b" -> "a%2Fb"
		url.QueryEscape(in.GetGroup()),
		url.QueryEscape(in.GetKey()),
	)
	res, err := http.Get(u)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned: %v", res.Status)
	}
	bytes, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("reading respones body: %v", err)
	}
	if err = proto.Unmarshal(bytes, out); err != nil {
		return fmt.Errorf("decoding response body: %v", err)
	}
	return nil
}
