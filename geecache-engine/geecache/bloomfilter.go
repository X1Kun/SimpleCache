package geecache

import "hash/fnv"

type BloomFilter struct {
	bitset []uint64 // 位图（BitMap）
	size   uint64   // 位图的长度
	hashes uint     // 使用的哈希函数的数量
}

func NewBloomFilter(size uint64, hashes uint) *BloomFilter {
	return &BloomFilter{
		// 如果需要100个bit，因为一个uint64占64位，所以需要 100/64+1=2 个uint64
		bitset: make([]uint64, (size/64)+1),
		size:   size,
		hashes: hashes,
	}
}

func hashFn(data []byte, seed uint) uint64 {
	h := fnv.New64a()
	h.Write(data)

	// 将哈希结果与种子进行混合，达到多哈希函数的效果
	return h.Sum64() + uint64(seed)*131313
}

// Add 将一个 Key 的“指纹”录入布隆过滤器
func (bf *BloomFilter) Add(key string) {
	for i := uint(0); i < bf.hashes; i++ {
		// 计算出当前哈希函数对应的 bit 位置
		idx := hashFn([]byte(key), i) % bf.size
		// 把对应的 bit 位标记为 1
		// idx/64 找到是哪个 uint64， idx%64 找到是这个 uint64 里的第几个 bit
		bf.bitset[idx/64] |= 1 << (idx % 64)
	}
}

// Contains 判断一个 Key 是否“可能存在”
func (bf *BloomFilter) Contains(key string) bool {
	for i := uint(0); i < bf.hashes; i++ {
		idx := hashFn([]byte(key), i) % bf.size

		// 检查对应的 bit 位是不是 0
		// 如果有任何一个哈希函数算出来的位置是 0，说明这个 Key 绝对不存在！
		if bf.bitset[idx/64]&(1<<(idx%64)) == 0 {
			return false
		}
	}
	// 所有的位置都是 1，说明它“大概率”存在
	return true
}
