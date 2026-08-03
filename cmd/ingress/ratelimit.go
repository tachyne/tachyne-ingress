package main

import (
	"sync"
	"time"
)

// limiter caps connection load at the edge. Three independent, best-effort
// limits, each disabled when its bound is zero:
//
//   - a global ceiling on concurrent connections (bounds total goroutines and
//     memory, so a flood can't OOM the pod and cycle the whole front door);
//   - a per-source-IP ceiling on concurrent connections (one IP can't hog the
//     global pool or slow-loris a fleet of held sessions);
//   - a per-source-IP new-connection rate (token bucket) that smooths bursts.
//
// It guards the Java TCP accept loop, where the source IP is real: a TCP
// connection only reaches Accept after the handshake completes, so the address
// can't be spoofed. Bedrock UDP uses a simpler global session cap (udp.go),
// because UDP source addresses ARE spoofable and per-IP limits there are moot.
type limiter struct {
	globalMax int
	perIPMax  int
	rate      float64 // token refill per second for the per-IP new-conn bucket
	burst     float64 // per-IP bucket capacity (also the initial full amount)

	mu      sync.Mutex
	global  int
	perIP   map[string]int
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(globalMax, perIPMax int, rate, burst float64) *limiter {
	return &limiter{
		globalMax: globalMax,
		perIPMax:  perIPMax,
		rate:      rate,
		burst:     burst,
		perIP:     map[string]int{},
		buckets:   map[string]*tokenBucket{},
	}
}

// acquire admits a new connection from ip, or reports ok=false to drop it. On
// success it returns release, which MUST be called exactly once when the
// connection ends (extra calls are no-ops). Each zero bound skips its check.
func (l *limiter) acquire(ip string) (release func(), ok bool) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.globalMax > 0 && l.global >= l.globalMax {
		return nil, false
	}
	if l.perIPMax > 0 && l.perIP[ip] >= l.perIPMax {
		return nil, false
	}
	// Rate is checked last so a rejected connection never consumes a token on
	// behalf of a limit it already failed.
	if l.rate > 0 && !l.takeToken(ip, now) {
		return nil, false
	}

	l.global++
	l.perIP[ip]++
	var released bool
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if released {
			return
		}
		released = true
		l.global--
		if n := l.perIP[ip] - 1; n <= 0 {
			delete(l.perIP, ip) // reclaim the map slot when an IP goes quiet
		} else {
			l.perIP[ip] = n
		}
	}, true
}

// takeToken refills ip's bucket toward burst by the elapsed time and consumes
// one token, reporting whether one was available. Caller holds l.mu.
func (l *limiter) takeToken(ip string, now time.Time) bool {
	b := l.buckets[ip]
	if b == nil {
		if l.burst < 1 {
			// A burst below one token can never admit anything; treat it as a
			// misconfiguration rather than silently rejecting every client.
			return true
		}
		// A fresh IP starts with a full bucket, then spends one token.
		l.buckets[ip] = &tokenBucket{tokens: l.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepBuckets periodically drops idle, fully-refilled token buckets so the map
// can't grow without bound as distinct IPs come and go. A dropped bucket is
// indistinguishable from the fresh full one a returning IP would get, so
// nothing is lost.
func (l *limiter) sweepBuckets(every time.Duration) {
	for range time.Tick(every) {
		now := time.Now()
		l.mu.Lock()
		for ip, b := range l.buckets {
			b.tokens += now.Sub(b.last).Seconds() * l.rate
			if b.tokens >= l.burst {
				delete(l.buckets, ip)
			}
		}
		l.mu.Unlock()
	}
}
