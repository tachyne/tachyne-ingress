// Command ingress is the tachyne cluster's single hardened public entrypoint.
//
// Java (TCP): it reads exactly one packet — the handshake, which carries the
// client's protocol version — routes the connection to the matching
// version-pinned gateway, replays the handshake, and splices the streams.
// Unknown versions get an honest local answer (status or login disconnect)
// naming what's supported.
//
// Bedrock (UDP/RakNet): it forwards datagrams to the internal Bedrock gateway
// service (per-client session table; see udp.go). The gateway pods are no
// longer publicly reachable — everything enters here so it can be hardened in
// one place. The gateways are the cells; ingress is the carrier frequency.
//
// Env:
//
//	INGRESS_LISTEN      TCP listen address                 (default ":25565")
//	INGRESS_ROUTES      "770-772=host:port,776=host:port"  (required)
//	INGRESS_SUPPORTED   human list for unknown versions    (default derived)
//	INGRESS_PROXY       "1" = send PROXY protocol v1 line  (default off until
//	                    gateways parse it)
//	INGRESS_BEDROCK_LISTEN   UDP listen address            (default ":19132")
//	INGRESS_BEDROCK_BACKEND  Bedrock gateway addr host:port ("" = Bedrock off)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tachyne/tachyne-common/protocol"
)

type route struct {
	lo, hi int32
	addr   string
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	listen := envOr("INGRESS_LISTEN", ":25565")
	routes, err := parseRoutes(os.Getenv("INGRESS_ROUTES"))
	if err != nil || len(routes) == 0 {
		log.Fatalf("INGRESS_ROUTES: %v", err)
	}
	supported := envOr("INGRESS_SUPPORTED", "1.21.5-1.21.8 and 26.2")
	sendProxy := os.Getenv("INGRESS_PROXY") == "1"

	// Edge IP firewall via tachyne-access (optional; empty URL = allow all).
	guard := newIPGuard(os.Getenv("INGRESS_ACCESS_URL"), os.Getenv("INGRESS_ACCESS_TOKEN"))
	if guard.url != "" {
		log.Printf("edge IP firewall on (tachyne-access %s)", guard.url)
	}

	// Bedrock (UDP) front door — optional; only runs if a backend is set.
	if backend := os.Getenv("INGRESS_BEDROCK_BACKEND"); backend != "" {
		go serveBedrock(envOr("INGRESS_BEDROCK_LISTEN", ":19132"), backend, guard)
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("tachyne-ingress (java) on %s, %d routes, proxy-protocol=%v", listen, len(routes), sendProxy)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		// Cheap fast-path: a known-blocked IP is dropped here, before spawning a
		// goroutine or reading a byte, so a repeat offender costs almost nothing.
		if guard.blocked(hostOf(c.RemoteAddr())) {
			c.Close()
			continue
		}
		go handle(c, routes, supported, sendProxy, guard)
	}
}

func handle(c net.Conn, routes []route, supported string, sendProxy bool, guard *ipGuard) {
	defer c.Close()
	// Edge firewall: drop a blocked source before any protocol handling.
	if !guard.allow(hostOf(c.RemoteAddr())) {
		return
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	if _, err := br.Peek(1); err != nil {
		return // TCP probe
	}
	pkt, err := protocol.ReadPacket(br)
	if err != nil || pkt.ID != 0x00 {
		return
	}
	r := pkt.Body()
	proto, err := protocol.ReadVarInt(r)
	if err != nil {
		return
	}
	if _, err := protocol.ReadString(r); err != nil { // server address
		return
	}
	if _, err := io.CopyN(io.Discard, r, 2); err != nil { // port
		return
	}
	intent, err := protocol.ReadVarInt(r)
	if err != nil {
		return
	}

	for _, rt := range routes {
		if proto >= rt.lo && proto <= rt.hi {
			if intent != 1 { // don't log server-list pings
				log.Printf("%s: proto %d intent %d → %s", c.RemoteAddr(), proto, intent, rt.addr)
			}
			relay(c, br, pkt.Data, rt.addr, sendProxy)
			return
		}
	}
	// No gateway speaks this version — answer locally, honestly.
	log.Printf("%s: unsupported protocol %d (intent %d)", c.RemoteAddr(), proto, intent)
	switch intent {
	case 1:
		serveStatus(c, br, proto, supported)
	case 2, 3:
		msg, _ := json.Marshal(map[string]string{"text": fmt.Sprintf(
			"No gateway speaks your client's protocol (%d).\nSupported: Minecraft %s.", proto, supported)})
		protocol.WritePacket(c, 0x00, protocol.AppendString(nil, string(msg)))
	}
}

// relay connects to the gateway, replays the handshake, and splices.
func relay(c net.Conn, br *bufio.Reader, handshakeBody []byte, addr string, sendProxy bool) {
	g, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		log.Printf("%s: gateway %s unreachable: %v", c.RemoteAddr(), addr, err)
		return
	}
	defer g.Close()
	if sendProxy {
		src := c.RemoteAddr().(*net.TCPAddr)
		dst := c.LocalAddr().(*net.TCPAddr)
		fmt.Fprintf(g, "PROXY TCP4 %s %s %d %d\r\n", src.IP, dst.IP, src.Port, dst.Port)
	}
	if err := protocol.WritePacket(g, 0x00, handshakeBody); err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { io.Copy(g, br); done <- struct{}{} }() // client → gateway (incl. buffered residue)
	go func() { io.Copy(c, g); done <- struct{}{} }()  // gateway → client
	<-done                                             // either side closing ends the session; defers close the other
}

// serveStatus answers a status ping for an unsupported version locally.
func serveStatus(c net.Conn, br *bufio.Reader, proto int32, supported string) {
	for {
		pkt, err := protocol.ReadPacket(br)
		if err != nil {
			return
		}
		switch pkt.ID {
		case 0x00:
			st := map[string]any{
				// Echo the client's protocol so the MOTD is readable in the list.
				"version":     map[string]any{"name": "tachyne", "protocol": proto},
				"players":     map[string]any{"max": 0, "online": 0},
				"description": map[string]any{"text": "tachyne — supported: Minecraft " + supported},
			}
			payload, _ := json.Marshal(st)
			if protocol.WritePacket(c, 0x00, protocol.AppendString(nil, string(payload))) != nil {
				return
			}
		case 0x01:
			protocol.WritePacket(c, 0x01, pkt.Data)
			return
		default:
			return
		}
	}
}

func parseRoutes(s string) ([]route, error) {
	var out []route
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		spec, addr, ok := strings.Cut(part, "=")
		if !ok || addr == "" {
			return nil, fmt.Errorf("bad route %q", part)
		}
		lo, hi, _ := strings.Cut(spec, "-")
		l, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("bad route %q", part)
		}
		h := l
		if hi != "" {
			if h, err = strconv.Atoi(hi); err != nil {
				return nil, fmt.Errorf("bad route %q", part)
			}
		}
		out = append(out, route{lo: int32(l), hi: int32(h), addr: addr})
	}
	return out, nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
