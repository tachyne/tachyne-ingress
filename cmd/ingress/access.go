package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// ipGuard is the edge firewall: it asks tachyne-access whether a bare source IP
// may connect, before any protocol handling. It's the ONE place client IPs are
// visible (Java TCP source, Bedrock UDP source) — identity authz stays at the
// gateways, which is the only place a decrypted identity exists (Bedrock's login
// is encrypted inside RakNet; the ingress can never see it).
//
// Verdicts are cached briefly. It FAILS OPEN: if access is unreachable the
// connection proceeds to the gateway, which does its own fail-CLOSED identity
// check — so a wobbly access service never locks the door by itself, and a real
// deny still lands as soon as access answers again.
type ipGuard struct {
	url   string // access base URL ("" = guard disabled, allow all)
	token string
	http  *http.Client

	mu    sync.Mutex
	cache map[string]cachedVerdict
}

type cachedVerdict struct {
	allow bool
	exp   time.Time
}

const (
	ipAllowTTL     = 30 * time.Second // allows expire quickly (a new grant applies fast)
	ipDenyTTL      = 5 * time.Minute  // denies stick — a repeat offender rarely re-hits access
	ipFailTTL      = 5 * time.Second  // shorter cache when access errored (fail-open)
	ipCheckTimeout = 3 * time.Second
)

func newIPGuard(url, token string) *ipGuard {
	if url == "" {
		return &ipGuard{} // disabled
	}
	return &ipGuard{
		url:   url,
		token: token,
		http:  &http.Client{Timeout: ipCheckTimeout},
		cache: map[string]cachedVerdict{},
	}
}

// allow blocks on a cache miss to fetch a verdict — use from a per-connection
// goroutine (Java). Disabled guard or any access error ⇒ allow (fail open).
func (g *ipGuard) allow(ip string) bool {
	if g.url == "" {
		return true
	}
	if v, ok := g.cached(ip); ok {
		return v
	}
	return g.fetch(ip)
}

// allowCached never blocks: it answers from cache, and on a miss it allows this
// datagram through while refreshing the verdict in the background. Use from the
// single-threaded Bedrock UDP loop, where a synchronous HTTP call would stall
// every client. A denied IP may get one brief session before the deny caches.
func (g *ipGuard) allowCached(ip string) bool {
	if g.url == "" {
		return true
	}
	if v, ok := g.cached(ip); ok {
		return v
	}
	go g.fetch(ip)
	return true
}

func (g *ipGuard) cached(ip string) (bool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.cache[ip]; ok && time.Now().Before(c.exp) {
		return c.allow, true
	}
	return false, false
}

// blocked reports a CACHED deny only — never blocks, never hits access. The TCP
// accept loop uses it to drop a known offender before spawning a goroutine, so a
// hammering banned IP costs almost nothing (and the long ipDenyTTL keeps it that
// way without re-querying access).
func (g *ipGuard) blocked(ip string) bool {
	if g.url == "" {
		return false
	}
	allow, ok := g.cached(ip)
	return ok && !allow
}

func (g *ipGuard) store(ip string, allow bool, ttl time.Duration) {
	g.mu.Lock()
	g.cache[ip] = cachedVerdict{allow: allow, exp: time.Now().Add(ttl)}
	g.mu.Unlock()
}

func (g *ipGuard) fetch(ip string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), ipCheckTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"ip": ip})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/v1/check-ip", bytes.NewReader(body))
	if err != nil {
		g.store(ip, true, ipFailTTL)
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		log.Printf("ip-check %s: access unreachable (%v) — failing open", ip, err)
		g.store(ip, true, ipFailTTL)
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("ip-check %s: access status %d — failing open", ip, resp.StatusCode)
		g.store(ip, true, ipFailTTL)
		return true
	}
	var v struct {
		Allow  bool   `json:"allow"`
		Reason string `json:"reason"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		g.store(ip, true, ipFailTTL)
		return true
	}
	ttl := ipAllowTTL
	if !v.Allow {
		ttl = ipDenyTTL // cache denials long so repeat offenders don't re-hit access
		log.Printf("ip-check %s: DENIED (%s)", ip, v.Reason)
	}
	g.store(ip, v.Allow, ttl)
	return v.Allow
}

// hostOf extracts the IP from a host:port address.
func hostOf(addr net.Addr) string {
	if h, _, err := net.SplitHostPort(addr.String()); err == nil {
		return h
	}
	return addr.String()
}
