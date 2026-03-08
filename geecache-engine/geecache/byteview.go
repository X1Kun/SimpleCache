package geecache

// 结构体包装[]byte
type ByteView struct {
	b []byte
}

// 实现了Value的Len()接口
func (bv ByteView) Len() int {
	return len(bv.b)
}

// 返回一个新建的切片
func (bv ByteView) ByteSlice() []byte {
	return cloneBytes(bv.b)
}

// []byte转化为string
func (bv ByteView) String() string {
	return string(bv.b)
}

// 深拷贝切片
func cloneBytes(input []byte) []byte {
	output := make([]byte, len(input))
	copy(output, input)
	return output
}
