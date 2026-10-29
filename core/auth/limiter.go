package auth

// In-memory login throttle: N failures within a window → cooldown.
// Single-instance per tenant, so process memory is the right scope.
//
// 内存级登录限速：窗口内失败 N 次 → 冷却。
// 每租户单实例部署，进程内计数即可。

import (
	"sync"
	"time"
)

type Limiter struct {
	mu      sync.Mutex
	fails   map[string]*failRec
	maxFail int
	window  time.Duration
	lock    time.Duration
}

type failRec struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

func NewLimiter() *Limiter {
	return &Limiter{
		fails:   make(map[string]*failRec),
		maxFail: 5,
		window:  5 * time.Minute,
		lock:    15 * time.Minute,
	}
}

// Allow reports whether an attempt may proceed.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.fails[key]
	if !ok {
		return true
	}
	now := time.Now()
	if now.Before(r.lockedUntil) {
		return false
	}
	if now.Sub(r.first) > l.window {
		delete(l.fails, key)
	}
	return true
}

// Fail records a failed attempt; returns whether the key is now locked.
func (l *Limiter) Fail(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	r, ok := l.fails[key]
	if !ok || now.Sub(r.first) > l.window {
		r = &failRec{first: now}
		l.fails[key] = r
	}
	r.count++
	if r.count >= l.maxFail {
		r.lockedUntil = now.Add(l.lock)
		return true
	}
	return false
}

// Reset clears failures after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
