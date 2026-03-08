package main

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// 并发客户端数量
	// Concurrency = 100
	Concurrency = 100
	// 每个客户端发送的请求数
	// RequestsPerWorker = 200
	RequestsPerWorker = 1000
	// 目标网关地址
	TargetURL = "http://localhost:9999/api?key="
)

func main() {
	var wg sync.WaitGroup
	var successCount int32
	var failCount int32

	// 我们故意只用 10 个 Key，以此来测试缓存命中率和 SingleFlight 防击穿能力
	// keys := []string{"Tom", "Jack", "Sam", "Alice", "Bob", "Cindy", "David", "Eva", "Frank", "Grace"}

	fmt.Printf("🚀 压测开始: %d 个并发客户端，共计发送 %d 次请求...\n", Concurrency, Concurrency*RequestsPerWorker)
	startTime := time.Now()

	for i := 0; i < Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// 每个 worker 疯狂发请求
			for j := 0; j < RequestsPerWorker; j++ {
				// 轮询使用这 10 个 key
				// key := keys[j%len(keys)]
				// key := fmt.Sprintf("Auto-%d-%d", workerID, j)
				key := fmt.Sprintf("Auto-%d", rand.Intn(1000000))
				url := TargetURL + key
				// url = TargetURL + "Tom"

				resp, err := http.Get(url)
				if err != nil {
					atomic.AddInt32(&failCount, 1)
					continue
				}

				// 必须读取并关闭 Body，否则会耗尽本地连接池导致压测程序自己崩溃
				io.ReadAll(resp.Body)
				resp.Body.Close()

				if resp.StatusCode == http.StatusOK {
					atomic.AddInt32(&successCount, 1)
				} else {
					atomic.AddInt32(&failCount, 1)
				}
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(startTime)

	// 打印压测报告
	fmt.Println("=====================================")
	fmt.Println("📊 压测报告出炉！")
	fmt.Printf("⏱️  总耗时: %v\n", duration)
	fmt.Printf("✅ 成功请求: %d\n", successCount)
	fmt.Printf("❌ 失败请求: %d\n", failCount)
	fmt.Printf("⚡ QPS (每秒查询率): %.2f\n", float64(successCount+failCount)/duration.Seconds())
	fmt.Println("=====================================")
}
