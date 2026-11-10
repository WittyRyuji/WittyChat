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
	r, ok := l.fails[key]
	fresh := !ok || now.Sub(r.first) > l.window ||
		(!r.lockedUntil.IsZero() && !now.Before(r.lockedUntil))
	if fresh {
		r = &failRec{first: now}
		l.fails[key] = r
	}
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
// a lockout: spraying an account can't deny its owner (who can also be
// exempted upstream via a known-device cookie).
// Schedule 按键串行化尝试，间隔随失败数增长：调用方原子地预订下一个
// 时点，N 个并发请求被摊到 N×间隔上而非并行验证。返回（等待，可用）；
// ok=false 表示队列已超出 maxWait——直接 429 而非挂起请求。这是减速
// 不是锁定：泼水的攻击者不能把真实属主拒之门外（属主还可凭"已知设备"
// cookie 在上游豁免）。
func (l *Limiter) Schedule(key string, maxWait time.Duration) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.sweepLocked(now)
	r, ok := l.fails[key]
	if !ok || now.Sub(r.first) > l.window {
		r = &failRec{first: now}
		l.fails[key] = r
	}
	slot := now
	if r.nextSlot.After(now) {
		slot = r.nextSlot
	}
	wait := slot.Sub(now)
	if wait > maxWait {
		return 0, false
	}
	// Gap: a short floor serializes concurrent verifies; past the
	// failure threshold each guess pays (count-maxFail) extra seconds.
	// 间隔：短底限先把并发验证串起来；失败数越过后每次猜测多付
	// (count-maxFail) 秒。
	gap := 150 * time.Millisecond
	if over := r.count - l.maxFail + 1; over > 0 {
		gap = time.Duration(over) * time.Second
		if gap > 15*time.Second {
			gap = 15 * time.Second
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
