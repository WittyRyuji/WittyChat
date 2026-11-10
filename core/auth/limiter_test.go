package auth

// Unit tests for the login throttle semantics — no DB needed.
// 登录限速器语义单测——无需数据库。

import (
	"sync"
	"testing"
	"time"
)

// Attempt must be atomic: a burst of concurrent attempts on one key gets
// at most maxFail admissions — the old check-then-count let every
// goroutine pass before any failure was recorded.
// Attempt 必须原子：同一 key 上的并发突发最多放行 maxFail 次——旧实现
// 先查后记，所有协程都能在计数前通过。
func TestAttemptConcurrentBurst(t *testing.T) {
	l := NewLimiter(3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Attempt("k") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 3 {
		t.Fatalf("burst admitted %d attempts, want 3", allowed)
	}
	// Once locked, further attempts stay denied while the lock holds.
	if l.Attempt("k") {
		t.Fatal("locked key admitted an attempt")
	}
}

// Lock expiry gives a clean window — a locked key must not re-lock on
// the very next attempt forever.
// 锁定到期即重置——不能让锁定的键在下一次尝试时永久再锁。
func TestAttemptLockExpiry(t *testing.T) {
	l := NewLimiter(2)
	l.Configure(2, 50*time.Millisecond, 50*time.Millisecond)
	l.Attempt("k")
	l.Attempt("k")
	if l.Attempt("k") {
		t.Fatal("threshold+1 admitted")
	}
	time.Sleep(60 * time.Millisecond)
	if !l.Attempt("k") {
		t.Fatal("expired lock did not reset the window")
	}
}

// Schedule serializes attempts: concurrent callers get distinct slots
// spaced by the gap, and the queue rejects beyond maxWait.
// Schedule 串行化尝试：并发调用方拿到间隔错开的时隙，超过 maxWait 拒绝。
func TestScheduleSerializes(t *testing.T) {
	l := NewLimiter(3)
	wait1, ok1 := l.Schedule("a", time.Second)
	wait2, ok2 := l.Schedule("a", time.Second)
	wait3, ok3 := l.Schedule("a", time.Second)
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("schedule rejected early: %v %v %v", ok1, ok2, ok3)
	}
	if wait1 != 0 || wait2 < 100*time.Millisecond || wait3 < 200*time.Millisecond {
		t.Fatalf("slots not serialized: %v %v %v", wait1, wait2, wait3)
	}
	// Saturate the queue past maxWait → rejection.
	for i := 0; i < 50; i++ {
		if _, ok := l.Schedule("a", 50*time.Millisecond); !ok {
			return
		}
	}
	t.Fatal("saturated queue still admitted attempts")
}

// Failure count grows the serialized gap — the spray path gets slower,
// not blocked.
// 失败数拉大排队间隔——刷猜变慢而非封死。
func TestScheduleGapGrowsWithFailures(t *testing.T) {
	l := NewLimiter(2)
	l.Fail("a")
	l.Fail("a")
	l.Fail("a")
	w1, _ := l.Schedule("a", 30*time.Second)
	w2, _ := l.Schedule("a", 30*time.Second)
	if w2-w1 < 1*time.Second {
		t.Fatalf("gap did not grow with failures: %v → %v", w1, w2)
	}
}

// Sweep is amortized — crossing the key threshold doesn't make every call
// walk the whole map (the old per-call full scan was quadratic under
// attacker-filled keys). / 清扫摊销——越过键数阈值不再让每次调用都扫全表
// （旧的逐调用全扫在攻击者灌键下是平方级）。
func TestSweepAmortized(t *testing.T) {
	l := NewLimiter(5)
	for i := 0; i < limiterSweepAt+50; i++ {
		l.fails[string(rune('a'+i%26))+string(rune(i))] = &failRec{first: time.Now().Add(-time.Hour)}
	}
	l.sweptAt = time.Now() // pretend a sweep just ran / 假装刚扫过
	start := time.Now()
	for i := 0; i < 1000; i++ {
		l.Attempt("newkey")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("1000 attempts took %v — per-call sweep not amortized", d)
	}
}

// Hard cap: a fresh key always gets tracked — one arbitrary record is
// evicted instead of letting the map grow unbounded.
// 硬上限：新键总能登记——逐出一条任意记录腾位，而不是放任表无限增长。
func TestHardCapEvicts(t *testing.T) {
	l := NewLimiter(5)
	for i := 0; i < limiterHardCap; i++ {
		l.fails[string(rune(i))] = &failRec{first: time.Now()}
	}
	if !l.Attempt("fresh-key") {
		t.Fatal("fresh key rejected at cap")
	}
	if len(l.fails) > limiterHardCap {
		t.Fatalf("map grew past cap: %d", len(l.fails))
	}
}
