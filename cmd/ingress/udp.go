package main

import (
	"log"
	"net"
	"sync"
	"time"
)

// Bedrock is RakNet over UDP, so there is no handshake to route on and no
// stream to splice — ingress is a stateful UDP forwarder in front of the
// single internal Bedrock gateway service. For each distinct client address it
// opens a dedicated socket to the backend (a NAT-style session) and pumps
// datagrams both ways; idle sessions are reaped. RakNet keeps a session pinned
// to one path via its own connection handshake + keepalives, so as long as a
// given client always reaches the same ingress pod (Service sessionAffinity:
// ClientIP) the RakNet session is stable.
//
// The backend sees ingress as the peer (UDP SNAT): Bedrock identity comes from
// the XBL login (name/XUID), not the socket address, so access checks still
// work; hardening (rate limits, allow-lists) lives HERE, where the real client
// address is visible. Preserving the real client IP all the way to the gateway
// would need a PROXY-protocol-for-UDP shim on both ends — a later upgrade.

const (
	bedrockBufSize     = 2048             // > RakNet MTU (≤1492 negotiated)
	bedrockIdleTimeout = 60 * time.Second // RakNet keepalive is ~5s; 60s idle = gone
	bedrockSweepEvery  = 15 * time.Second
)

type bedrockSession struct {
	backend *net.UDPConn // ingress → gateway socket for this client
	client  *net.UDPAddr // where to send gateway → client datagrams
	last    time.Time    // last client activity (for idle reaping)
}

// serveBedrock runs the UDP front door until the process exits.
func serveBedrock(listen, backendAddr string, guard *ipGuard) {
	baddr, err := net.ResolveUDPAddr("udp", backendAddr)
	if err != nil {
		log.Fatalf("INGRESS_BEDROCK_BACKEND %q: %v", backendAddr, err)
	}
	front, err := net.ListenUDP("udp", mustUDPAddr(listen))
	if err != nil {
		log.Fatalf("bedrock listen %s: %v", listen, err)
	}
	log.Printf("tachyne-ingress (bedrock) on %s → %s", listen, backendAddr)

	var mu sync.Mutex
	sessions := make(map[string]*bedrockSession)

	go sweepBedrock(&mu, sessions)

	buf := make([]byte, bedrockBufSize)
	for {
		n, caddr, err := front.ReadFromUDP(buf)
		if err != nil {
			log.Fatalf("bedrock read: %v", err)
		}
		key := caddr.String()

		mu.Lock()
		s := sessions[key]
		if s == nil {
			// Edge firewall for a new client (non-blocking; see allowCached).
			if !guard.allowCached(caddr.IP.String()) {
				mu.Unlock()
				continue // drop — no session, no RakNet reply
			}
			bconn, derr := net.DialUDP("udp", nil, baddr)
			if derr != nil {
				mu.Unlock()
				log.Printf("bedrock %s: dial backend failed: %v", key, derr)
				continue
			}
			s = &bedrockSession{backend: bconn, client: caddr, last: time.Now()}
			sessions[key] = s
			// gateway → client pump for this session
			go func() {
				rbuf := make([]byte, bedrockBufSize)
				for {
					rn, rerr := bconn.Read(rbuf)
					if rerr != nil {
						return // socket closed by the sweeper
					}
					front.WriteToUDP(rbuf[:rn], caddr)
				}
			}()
		} else {
			s.last = time.Now()
		}
		bconn := s.backend
		mu.Unlock()

		bconn.Write(buf[:n]) // client → gateway
	}
}

// sweepBedrock closes and removes sessions idle past the timeout; closing the
// backend socket unblocks that session's read pump so it returns.
func sweepBedrock(mu *sync.Mutex, sessions map[string]*bedrockSession) {
	for range time.Tick(bedrockSweepEvery) {
		cutoff := time.Now().Add(-bedrockIdleTimeout)
		mu.Lock()
		for key, s := range sessions {
			if s.last.Before(cutoff) {
				s.backend.Close()
				delete(sessions, key)
			}
		}
		mu.Unlock()
	}
}

func mustUDPAddr(addr string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Fatalf("bad udp addr %q: %v", addr, err)
	}
	return a
}
