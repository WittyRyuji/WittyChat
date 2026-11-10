package auth

// In-memory login throttle: N failures within a window → cooldown.
// Single-instance per tenant, so process memory is the right scope.
// The map never scans per call (an attacker filling it once turned every
// login into a full-table walk under the mutex — quadratic pressure):
// sweeps run at most every limiterSweepEvery, and past a hard cap one
// arbitrary record is evicted to make room — attacker keys mostly evict
// each other.
//
// 内存级登录限速：窗口内失败 N 次 → 冷却。每租户单实例，进程内计数。
// 表不再按调用全扫（攻击者灌满后每次登录都在锁内走全表——平方级压力）：
// 清扫最多每 limiterSweepEvery 一次；超硬上限时逐出一条任意记录腾位——
// 被逐的大多是攻击者自己的键。

import (
	"sync"
	"time"
)

// limiterSweepAt triggers the periodic sweep; limiterHardCap bounds the
// map; limiterSweepEvery throttles full scans. 触发周期清扫的表大小、
// 表硬上限、全扫最小间隔。
const (
	limiterSweepAt    = 8192
	limiterHardCap    = 65536
	limiterSweepEvery = 30 * time.Second
)

type Limiter struct {
	mu      sync.Mutex
	fails   map[string]*failRec
	maxFail int
	window  time.Duration
	lock    time.Duration
	sweptAt time.Time
}

type failRec struct {
	count       int
	first       time.Time
	lockedUntil time.Time
	nextSlot    time.Time // serialized attempt schedule (Schedule) / 串行化的尝试时点
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

// sweepLocked drops expired records, throttled to one full scan per
// limiterSweepEvery. Caller holds l.mu. 持锁调用；周期清扫过期记录。
func (l *Limiter) sweepLocked(now time.Time) {
	if len(l.fails) < limiterSweepAt || now.Sub(l.sweptAt) < limiterSweepEvery {
		return
	}
	l.sweptAt = now
	for k, r := range l.fails {
		if now.After(r.lockedUntil) && now.Sub(r.first) > l.window {
			delete(l.fails, k)
		}
	}
}

// recLocked returns the key's record, creating (or refreshing a stale
// one) as needed. At the hard cap one arbitrary record is evicted first —
// map order makes the victim effectively random. 返回键的记录；达到硬
// 上限先随机逐出一条腾位。
func (l *Limiter) recLocked(key string, now time.Time) *failRec {
	r, ok := l.fails[key]
	if ok && now.Sub(r.first) <= l.window && (r.lockedUntil.IsZero() || now.Before(r.lockedUntil)) {
		return r
	}
	if !ok && len(l.fails) >= limiterHardCap {
		for k := range l.fails {
			delete(l.fails, k)
			break
		}
	}
	r = &failRec{first: now}
	l.fails[key] = r
	return r
}

// Attempt atomically checks the key AND counts the attempt under one
// lock — the old check-then-count let concurrent requests all pass the
// check before any failure was recorded, so a burst could verify far
// more guesses than the threshold. false → key is locked (429).
// Attempt 在同一把锁内完成"检查+计数"——原先先查后记，并发请求在计数
// 落盘前全部放行，一波就能验证远超阈值的次数。false → 键已锁（429）。
func (l *Limiter) Attempt(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.sweepLocked(now)
	r := l.recLocked(key, now)
	if now.Before(r.lockedUntil) {
		return false
	}
	r.count++
	if r.count > l.maxFail {
		r.lockedUntil = now.Add(l.lock)
		return false
	}
	return true
}

// Schedule serializes attempts on a key with a gap that grows as
// failures accumulate: each caller reserves the next slot atomically, so
// N concurrent requests spread over N×gap instead of verifying in
// parallel. Returns (wait, ok); ok=false means the queue is already past
// maxWait — answer 429 instead of parking the request. A slowdown, not
// a lockout: known devices (a server-issued marker, not this key) skip
// the queue entirely so spraying an account can't deny its owner.
// Schedule 按键串行化尝试，间隔随失败数增长：调用方原子地预订下一个
// 时点，N 个并发请求被摊到 N×间隔上而非并行验证。返回（等待，可用）；
// ok=false 表示队列已超出 maxWait——直接 429 而非挂起请求。这是减速
// 不是锁定：已知设备（服务端签发的标记，与本键无关）整体跳过排队，
// 泼水的攻击者堵不住属主。
func (l *Limiter) Schedule(key string, maxWait time.Duration) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.sweepLocked(now)
	r := l.recLocked(key, now)
	slot := now
	if r.nextSlot.After(now) {
		slot = r.nextSlot
	}
	wait := slot.Sub(now)
	if wait > maxWait {
		return 0, false
	}
	// Gap: a short floor serializes concurrent verifies; past the
	// failure threshold each guess pays (count-maxFail)×2s extra.
	// 间隔：短底限先把并发验证串起来；失败数越过后每次猜测多付
	// (count-maxFail)×2 秒。
	gap := 150 * time.Millisecond
	if over := r.count - l.maxFail + 1; over > 0 {
		gap = time.Duration(over) * 2 * time.Second
		if gap > 20*time.Second {
			gap = 20 * time.Second
		}
	}
	r.nextSlot = slot.Add(gap)
	return wait, true
}

// Fail records a failed attempt; returns whether the key is now locked.
func (l *Limiter) Fail(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.sweepLocked(now)
	r := l.recLocked(key, now)
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
