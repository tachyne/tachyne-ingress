package main

import (
	"testing"
	"time"
)

func TestLimiterGlobalCap(t *testing.T) {
	l := newLimiter(2, 0, 0, 0) // global 2, others disabled
	r1, ok := l.acquire("a")
	if !ok {
		t.Fatal("1st should be admitted")
	}
	if _, ok := l.acquire("b"); !ok {
		t.Fatal("2nd should be admitted")
	}
	if _, ok := l.acquire("c"); ok {
		t.Fatal("3rd should hit the global cap")
	}
	r1() // free a slot
	if _, ok := l.acquire("c"); !ok {
		t.Fatal("slot freed, should be admitted")
	}
}

func TestLimiterPerIPCap(t *testing.T) {
	l := newLimiter(0, 2, 0, 0) // per-IP 2, others disabled
	r1, _ := l.acquire("a")
	r2, _ := l.acquire("a")
	if _, ok := l.acquire("a"); ok {
		t.Fatal("3rd from same IP should hit the per-IP cap")
	}
	if _, ok := l.acquire("b"); !ok {
		t.Fatal("a different IP should be unaffected")
	}
	r1()
	r2()
	l.mu.Lock()
	_, exists := l.perIP["a"]
	l.mu.Unlock()
	if exists {
		t.Fatal("per-IP counter should be reclaimed once the IP goes quiet")
	}
}

func TestLimiterReleaseIdempotent(t *testing.T) {
	l := newLimiter(1, 0, 0, 0)
	r, _ := l.acquire("a")
	r()
	r() // extra release must be a no-op, not underflow the counter
	if _, ok := l.acquire("b"); !ok {
		t.Fatal("counter should read 0 after release; slot available")
	}
	l.mu.Lock()
	g := l.global
	l.mu.Unlock()
	if g != 1 {
		t.Fatalf("global = %d, want 1 (no double-decrement)", g)
	}
}

func TestTokenBucketRefill(t *testing.T) {
	l := newLimiter(0, 0, 2, 3) // 2 tokens/sec, burst 3
	t0 := time.Unix(1000, 0)
	l.mu.Lock()
	defer l.mu.Unlock()

	// A fresh IP starts full (burst=3): three immediate takes, then dry.
	for i := 0; i < 3; i++ {
		if !l.takeToken("a", t0) {
			t.Fatalf("take %d should be granted from a full bucket", i+1)
		}
	}
	if l.takeToken("a", t0) {
		t.Fatal("4th immediate take should be denied (bucket empty)")
	}
	// After 1s, +2 tokens refilled.
	t1 := t0.Add(time.Second)
	if !l.takeToken("a", t1) || !l.takeToken("a", t1) {
		t.Fatal("two takes should be granted after a 1s refill")
	}
	if l.takeToken("a", t1) {
		t.Fatal("third take should be denied — only 2 refilled")
	}
	// Refill is capped at burst, not unbounded accumulation.
	t2 := t1.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if !l.takeToken("a", t2) {
			t.Fatalf("take %d after a long idle should be granted (bucket refilled to burst)", i+1)
		}
	}
	if l.takeToken("a", t2) {
		t.Fatal("bucket must cap at burst=3, not accumulate over the idle hour")
	}
}

// A burst below one token is a misconfiguration: it must not turn into "reject
// every connection after the first", which would lock out the whole server.
func TestLimiterBurstBelowOneAdmits(t *testing.T) {
	l := newLimiter(0, 0, 5, 0.5)
	for i := 0; i < 5; i++ {
		if _, ok := l.acquire("a"); !ok {
			t.Fatalf("connection %d rejected under a sub-token burst", i)
		}
	}
}
