package singleflight

import "sync"

// 存储每一个正在执行的请求
type call struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

// 存储所有的singleflight
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

// 根据一样的数据标签，决定是否要执行此函数 1、延迟初始化singleflight的组 2、查看singleflight中是否有对应的相同操作 3、有则等待，没有则创建call，并且执行取数逻辑 4、释放wg，返回结果
func (g *Group) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	g.mu.Lock()
	// 初始化
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}
