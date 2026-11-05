package auth

// In-memory login throttle: N failures within a window → cooldown.
// Single-instance per tenant, so process memory is the right scope.
// The map is swept once it grows past a bound so rotating keys can't
// grow it forever.
//
// 内存级登录限速：窗口内失败 N 次 → 冷却。
// 每租户单实例部署，进程内计数即可；表超过上限即清扫，轮换 key 不会
// 让内存无限增长。

import (
	"sync"
	"time"
)

// limiterSweepAt bounds the fail map; past it, expired/stale records are
// purged opportunistically on read/write. 失败表上限，超过即顺手清扫。
const limiterSweepAt = 8192

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

// NewLimiter locks a key after maxFail failures inside the window.
func NewLimiter(maxFail int) *Limiter {
	return &Limiter{
		fails:   make(map[string]*failRec),
		maxFail: maxFail,
		window:  5 * time.Minute,
		lock:    15 * time.Minute,
	}
}

// Configure adjusts the throttle policy (security.lock_* workspace
// settings). 调整限速策略（后台 security.lock_* 设置）。
func (l *Limiter) Configure(maxFail int, window, lock time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if maxFail > 0 {
		l.maxFail = maxFail
	}
	if window > 0 {
		l.window = window
	}
	if lock > 0 {
		l.lock = lock
	}
}

// sweepLocked drops records whose window has passed (including expired
// locks). Caller holds l.mu. 调用方持锁；清掉窗口已过的记录。
func (l *Limiter) sweepLocked(now time.Time) {
	if len(l.fails) < limiterSweepAt {
		return
	}
	for k, r := range l.fails {
		if now.After(r.lockedUntil) && now.Sub(r.first) > l.window {
			delete(l.fails, k)
		}
	}
}

// Allow reports whether an attempt may proceed.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.sweepLocked(now)
	r, ok := l.fails[key]
	if !ok {
		return true
	}
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
	l.sweepLocked(now)
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
