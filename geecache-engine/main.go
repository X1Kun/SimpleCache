package main

import (
	"flag"
	"fmt"
	"geecache"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

/*
$ curl http://localhost:9999/_geecache/scores/Tom
630

$ curl http://localhost:9999/_geecache/scores/kkk
kkk not exist
*/
var db = map[string]string{
	"Tom":  "630",
	"Jack": "589",
	"Sam":  "567",
}

func createGroup() *geecache.Group {
	return geecache.NewGroup("scores", 2<<10, geecache.GetterFunc(
		func(key string) ([]byte, error) {
			log.Println("[SlowDB] search key", key)

			// 模拟慢查询 -> 验证 SingleFlight
			time.Sleep(100 * time.Millisecond)

			// 如果是原来那三个，还返回原来的
			if v, ok := db[key]; ok {
				return []byte(v), nil
			}

			// 自动生成数据
			return []byte(fmt.Sprintf("Value-for-%s", key)), nil
		}))
}

func startCacheServer(selfAddr string, listenAddr string, addrs []string, gee *geecache.Group) {
	// 初始化HTTPPool，传自己的地址，用于哈希环计算
	peers := geecache.NewHTTPPool(selfAddr)
	// 将集群中所有节点放入哈希环
	peers.Set(addrs...)
	gee.RegisterPeers(peers)
	log.Printf("geecache is running. Identity: %s, Listening on: %s\n", selfAddr, listenAddr)

	log.Fatal(http.ListenAndServe(listenAddr, peers))
}

func startAPIServer(listenAddr string, gee *geecache.Group) {
	http.Handle("/api", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			key := r.URL.Query().Get("key")
			view, err := gee.Get(key)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(view.ByteSlice())

		}))
	// 暴露 metrics 接口供 Prometheus 抓取
	http.Handle("/metrics", promhttp.Handler())

	log.Println("fontend server is running at", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))

}

func main() {
	var port int
	var api bool
	flag.IntVar(&port, "port", 8002, "Geecache server port")
	flag.BoolVar(&api, "api", false, "Start a api server?")
	flag.Parse()

	selfAddr := os.Getenv("SELF_ADDR")
	if selfAddr == "" {
		// 降级方案：如果没有环境变量，默认回退到本地测试模式
		selfAddr = fmt.Sprintf("http://localhost:%d", port)
	}

	peersEnv := os.Getenv("PEERS")
	var peersList []string
	if peersEnv != "" {
		peersList = strings.Split(peersEnv, ",")
	} else {
		// 降级方案：本地单机测时的假数据
		peersList = []string{"http://localhost:8001", "http://localhost:8002", "http://localhost:8003"}
	}

	gee := createGroup()
	if api {
		// API 暴露给外部用户，监听 0.0.0.0
		go startAPIServer("0.0.0.0:9999", gee)
	}

	listenAddr := fmt.Sprintf("0.0.0.0:%d", port)
	startCacheServer(selfAddr, listenAddr, peersList, gee)
}
