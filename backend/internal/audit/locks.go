package audit

import "sync"

// sessionLocks 是按会话目录（相对路径）加锁的读写锁表。
//
// 写入方（请求落盘：创建文件 → 压缩写入 → 关闭）持读锁，同一会话的多个请求互不阻塞；
// 夜间归档持写锁，并且只用 TryLock：会话正在写入即说明仍活跃，本轮直接跳过。
// 条目按引用计数回收，表的大小只与“正在写入/归档的会话数”有关。
type sessionLocks struct {
	mu sync.Mutex
	m  map[string]*sessionLock
}

type sessionLock struct {
	rw   sync.RWMutex
	refs int
}

func newSessionLocks() *sessionLocks {
	return &sessionLocks{m: make(map[string]*sessionLock)}
}

func (l *sessionLocks) acquire(key string) *sessionLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[key]
	if e == nil {
		e = &sessionLock{}
		l.m[key] = e
	}
	e.refs++
	return e
}

func (l *sessionLocks) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.m[key]; e != nil {
		e.refs--
		if e.refs <= 0 {
			delete(l.m, key)
		}
	}
}

// RLock 获取会话的写入锁（共享），返回解锁函数。
func (l *sessionLocks) RLock(key string) func() {
	e := l.acquire(key)
	e.rw.RLock()
	return func() {
		e.rw.RUnlock()
		l.release(key)
	}
}

// TryLock 尝试获取会话的归档锁（独占）；失败返回 nil。
func (l *sessionLocks) TryLock(key string) func() {
	e := l.acquire(key)
	if !e.rw.TryLock() {
		l.release(key)
		return nil
	}
	return func() {
		e.rw.Unlock()
		l.release(key)
	}
}
