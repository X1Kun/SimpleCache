package consistenthash

import (
	"fmt"
	"hash/crc32"
	"math"
	"strconv"
	"testing"
)

// 1. 测试数据迁移率
// 目标：验证当添加一台新机器时，是否只有约 1/N 的数据发生了迁移，而不是所有数据都变了。
func TestConsistency_Migration(t *testing.T) {
	// 使用真实的 CRC32 哈希算法
	hash := New(50, func(key []byte) uint32 {
		return crc32.ChecksumIEEE(key)
	})

	// 初始：3台机器
	hash.Add("Node-A", "Node-B", "Node-C")

	// 模拟：10000 个数据 Key
	keys := make([]string, 10000)
	for i := 0; i < 10000; i++ {
		keys[i] = strconv.Itoa(i)
	}

	// 记录：初始状态下，每个 Key 归谁管
	origLocation := make(map[string]string)
	for _, k := range keys {
		origLocation[k] = hash.Get(k)
	}

	// 动作：新加一台机器 Node-D
	hash.Add("Node-D")

	// 检查：有多少 Key 发生了移动？
	movedCount := 0
	for _, k := range keys {
		newLoc := hash.Get(k)
		if newLoc != origLocation[k] {
			movedCount++
		}
	}

	// 理论分析：
	// 原来有3台，现在4台。
	// 理论上应该有 1/4 (25%) 的数据会迁移到新节点 Node-D。
	// 剩下的 3/4 (75%) 的数据应该还在原来的 A, B, C 上不动。
	ratio := float64(movedCount) / float64(len(keys))

	t.Logf("Total Keys: 10000")
	t.Logf("Moved Keys: %d", movedCount)
	t.Logf("Migration Ratio: %.4f (Theory: 0.2500)", ratio)

	// 允许 10% 的误差
	if math.Abs(ratio-0.25) > 0.1 {
		t.Errorf("Migration ratio is too far from expected. Got %.4f, want ~0.25", ratio)
	}
}

// 2. 测试负载均衡（虚拟节点的作用）
// 目标：验证在大数据量下，数据是否比较均匀地分布在各个物理节点上。
func TestHashing_LoadBalance(t *testing.T) {
	// 如果 replicas 设置很小（比如 1），分布会极其不均匀
	// 设置为 50 或 100，分布就会趋于均匀
	virtualNodes := 50
	hash := New(virtualNodes, func(key []byte) uint32 {
		return crc32.ChecksumIEEE(key)
	})

	// 添加 3 台物理机器
	servers := []string{"Server-1", "Server-2", "Server-3"}
	hash.Add(servers...)

	// 模拟 10 万个请求
	requestCount := 100000
	serverCounts := make(map[string]int)

	for i := 0; i < requestCount; i++ {
		server := hash.Get(fmt.Sprintf("user_id_%d", i))
		serverCounts[server]++
	}

	// 打印结果并验证
	t.Logf("Testing with %d virtual nodes per server...", virtualNodes)
	for _, server := range servers {
		count := serverCounts[server]
		ratio := float64(count) / float64(requestCount)
		t.Logf("[%s] hits: %d (%.2f%%)", server, count, ratio*100)

		// 理论上每台机器应该分担 33.33%
		// 我们设置一个宽松的阈值，比如 25% ~ 40% 之间算正常
		if ratio < 0.25 || ratio > 0.40 {
			t.Errorf("Server %s load is unbalanced! ratio: %.2f", server, ratio)
		}
	}
}
