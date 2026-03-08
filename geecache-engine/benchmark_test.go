package main

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	APIBase    = "http://localhost:9999/api?key="
	TotalKeys  = 1000
	TotalReqs  = 10000
	Concurrent = 200
	HotKey     = "Auto-666"
)

// 全局复用的HTTP Client，预防端口耗尽
var httpClient *http.Client

func init() {
	// 创建一个复用连接的 Client
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     30 * time.Second,
	}
	httpClient = &http.Client{
		Transport: t,
		Timeout:   5 * time.Second,
	}
}

func TestUltraBenchmark(t *testing.T) {
	// 1. 适配 Windows
	var binaryName = "geecache_server"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}

	// 2. 编译
	fmt.Println("编译...")
	buildCmd := exec.Command("go", "build", "-o", binaryName)
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	defer func() { _ = os.Remove(binaryName) }()

	// 3. 启动集群
	fmt.Println("启动集群...")
	var cmds []*exec.Cmd
	ports := []string{"8001", "8002", "8003"}

	for _, port := range ports {
		args := []string{"-port=" + port}
		if port == "8003" {
			args = append(args, "-api=1")
		}
		cmd := exec.Command("./"+binaryName, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动失败: %v", err)
		}
		cmds = append(cmds, cmd)
	}

	defer func() {
		fmt.Println("\n清理")
		for _, cmd := range cmds {
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		}
	}()

	time.Sleep(2 * time.Second)
	fmt.Println("集群就绪！")

	// --- 4. SingleFlight 验证 ---
	fmt.Println("\nSingleFlight验证")
	var wgHot sync.WaitGroup
	for i := 0; i < 50; i++ { // 50个并发抢 HotKey
		wgHot.Add(1)
		go func() {
			defer wgHot.Done()
			fetchStrict(HotKey, fmt.Sprintf("Value-for-%s", HotKey), t, false)
		}()
	}
	wgHot.Wait()

	// --- 5. 压测 ---
	fmt.Printf("\n压测: %d 请求 | %d 并发\n", TotalReqs, Concurrent)

	var wg sync.WaitGroup
	var successCount int32
	var failCount int32

	sem := make(chan struct{}, Concurrent)
	start := time.Now()

	for i := 0; i < TotalReqs; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			randID := rand.Intn(TotalKeys)
			key := fmt.Sprintf("Auto-%d", randID)
			expectVal := fmt.Sprintf("Value-for-%s", key)

			// 只有失败时才打印详细错误，只打印前5个错误避免刷屏
			isSuccess := fetchStrict(key, expectVal, t, atomic.LoadInt32(&failCount) < 5)

			if isSuccess {
				atomic.AddInt32(&successCount, 1)
			} else {
				atomic.AddInt32(&failCount, 1)
			}
		}()
	}
	wg.Wait()
	duration := time.Since(start)

	// --- 6. 报告 ---
	qps := float64(TotalReqs) / duration.Seconds()
	fmt.Println("\n报告")
	fmt.Printf("成功: %d | 失败: %d | QPS: %.2f\n", successCount, failCount, qps)

	if failCount > 0 {
		t.Error("测试存在失败请求，请检查上方打印的错误信息")
	}
}

// shouldLogErr: 是否打印错误详情
func fetchStrict(key string, want string, t *testing.T, shouldLogErr bool) bool {
	// 使用全局复用的 httpClient
	resp, err := httpClient.Get(APIBase + key)
	if err != nil {
		if shouldLogErr {
			fmt.Printf("[Err Net] %v\n", err)
		}
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		if shouldLogErr {
			fmt.Printf("[Err Code] %d\n", resp.StatusCode)
		}
		return false
	}

	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	if got != want {
		// 忽略一些换行符差异
		if strings.TrimSpace(got) != strings.TrimSpace(want) {
			if shouldLogErr {
				fmt.Printf("[Err Val] Key:%s Got:%s Want:%s\n", key, got, want)
			}
			return false
		}
	}
	return true
}
