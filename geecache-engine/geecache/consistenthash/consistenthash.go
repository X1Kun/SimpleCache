package consistenthash

import (
	"hash/crc32"
	"sort"
	"strconv"
)

// 给哈希函数起一个方便理解的名字
type Hash func(data []byte) uint32

// Map相当于一个环
type Map struct {
	hash     Hash           // 将每一个虚拟桶映射在环上
	replicas int            // 每个物理桶的虚拟桶个数
	keys     []int          // 存储虚拟桶的哈希值
	hashMap  map[int]string // 虚拟桶哈希值映射回物理桶：n*replicas -> n
}

func New(replicas int, fn Hash) *Map {
	m := &Map{
		hash:     fn,
		replicas: replicas,
		hashMap:  make(map[int]string),
	}
	if m.hash == nil {
		m.hash = crc32.ChecksumIEEE
	}
	return m
}

// 为环添加节点，一个物理桶会引入replicas个虚拟桶 1.为每个虚拟桶计算哈希值，并存储在切片中 2.通过map将虚拟桶映射回物理桶 3、切片排序
func (m *Map) Add(keys ...string) {
	for _, key := range keys {
		for i := 0; i < m.replicas; i++ {
			// 变成：0key, 1key, 2key,并计算得到哈希值
			hash := int(m.hash([]byte(strconv.Itoa(i) + key)))
			// 将虚拟桶哈希值存在keys中，也就是刻在环上
			m.keys = append(m.keys, hash)
			// 通过虚拟桶哈希值,映射回物理桶
			m.hashMap[hash] = key
		}
	}
	sort.Ints(m.keys)
}

// 获得数据应存储的物理桶位置 1.计算数据标签的哈希值 2.返回第一个大于此哈希值的虚拟桶 3.虚拟桶映射回物理桶并返回
func (m *Map) Get(key string) string {
	if len(m.keys) == 0 {
		return ""
	}
	hash := int(m.hash([]byte(key)))
	// 返回第一个大于数据标签哈希值的虚拟桶index，如果无法找到，则返回n（这时候环就派上了用场）
	idx := sort.Search(len(m.keys), func(i int) bool {
		return m.keys[i] >= hash
	})
	// 通过index定位，得到虚拟桶的哈希值，映射回物理桶
	return m.hashMap[m.keys[idx%len(m.keys)]] // 大于keys最大值的数据和小于keys最小值的数据，都将分配在keys最小值的那个桶里
}
