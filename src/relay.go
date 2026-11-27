package main

import (
	"encoding/binary"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type tunnelCfg struct {
	maxConns      int
	maxHeader     int
	handshake     time.Duration
	maxWSBacklog  int
	maxUDPBacklog int
	vlessUDPHdr   bool
	heartbeat     time.Duration
	idle          time.Duration
	maxPayload    int64
	debug         bool
}

type tunnel struct {
	creds   *Creds
	cfg     tunnelCfg
	active  int32
	logf    func(string, ...any)
	failMu  sync.Mutex
	failMap map[string]*failRec
}

type failRec struct {
	n  int
	ts time.Time
}

func newTunnel(creds *Creds, cfg tunnelCfg, logf func(string, ...any)) *tunnel {
	return &tunnel{creds: creds, cfg: cfg, logf: logf, failMap: make(map[string]*failRec)}
}

func (t *tunnel) connCount() int { return int(atomic.LoadInt32(&t.active)) }

func (t *tunnel) noteFail(ip string) {
	t.failMu.Lock()
	defer t.failMu.Unlock()
	now := time.Now()
	rec := t.failMap[ip]
	if rec == nil || now.Sub(rec.ts) > 10*time.Minute {
		t.failMap[ip] = &failRec{n: 1, ts: now}
	} else {
		rec.n++
		rec.ts = now
	}
	if len(t.failMap) > 4096 {
		t.failMap = make(map[string]*failRec)
	}
}

func (t *tunnel) tooManyFails(ip string) bool {
	t.failMu.Lock()
	defer t.failMu.Unlock()
	rec := t.failMap[ip]
	if rec == nil || time.Since(rec.ts) > 10*time.Minute {
		return false
	}
	return rec.n >= 20
}

func (t *tunnel) handleSubscribe(w http.ResponseWriter, r *http.Request, getenv func(string) string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")

	ip := clientIP(r)
	if t.tooManyFails(ip) {
		http.Error(w, "Not Found", 404)
		return
	}
	if !safeEqual(r.URL.Query().Get("token"), t.creds.SubToken) {
		t.noteFail(ip)
		http.Error(w, "Not Found", 404)
		return
	}

	host := pickHost(r, getenv)
	port := envInt(getenv, "SUB_PORT", 443, 1, 65535)
	tls := "tls"
	if v := strings.ToLower(getenv("SUB_TLS")); v == "none" {
		tls = "none"
	}
	sni := host
	if v := strings.TrimSpace(getenv("SUB_SNI")); v != "" && hostRe.MatchString(v) {
		sni = strings.ToLower(v)
	}
	label := getenv("SUB_LABEL")
	if label == "" {
		label = "Crucix"
	}
	_, _, raw, b64 := buildSubscription(t.creds, host, port, tls, sni, label, strings.TrimSpace(getenv("SUB_ALPN")))

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("raw") == "1" {
		w.Write([]byte(raw))
		return
	}
	w.Write([]byte(b64))
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (t *tunnel) handleWS(w http.ResponseWriter, r *http.Request) {
	if atomic.AddInt32(&t.active, 1) > int32(t.cfg.maxConns) {
		atomic.AddInt32(&t.active, -1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ws, err := upgradeWS(w, r, t.cfg.maxPayload)
	if err != nil {
		atomic.AddInt32(&t.active, -1)
		return
	}

	q := newFrameQueue(16 * 1024 * 1024)
	ws.onClose = func() {
		q.Close()
		atomic.AddInt32(&t.active, -1)
	}
	ws.start()

	if t.cfg.heartbeat > 0 {
		go func() {
			tick := time.NewTicker(t.cfg.heartbeat)
			defer tick.Stop()
			for range tick.C {
				if ws.closed.Load() {
					return
				}
				if err := ws.Ping(); err != nil {
					return
				}
			}
		}()
	}

	var mu sync.Mutex
	hdrDone := false
	acc := make([]byte, 0, 512)
	finished := false

	deadline := time.AfterFunc(t.cfg.handshake, func() {
		mu.Lock()
		done := finished
		mu.Unlock()
		if !done {
			ws.Close()
		}
	})

	ws.onMessage = func(p []byte) {
		mu.Lock()
		if hdrDone {
			mu.Unlock()
			if q.Len() > t.cfg.maxWSBacklog {
				ws.Close()
				return
			}
			if !q.Push(p) {
				ws.Close()
			}
			return
		}
		if len(acc)+len(p) > t.cfg.maxHeader {
			mu.Unlock()
			ws.Close()
			return
		}
		acc = append(acc, p...)
		h := parseTunnelHeader(acc, t.creds)
		if h.Status == hdrNeed {
			if len(acc) > t.cfg.maxHeader {
				mu.Unlock()
				ws.Close()
				return
			}
			mu.Unlock()
			return
		}
		if h.Status == hdrBad {
			mu.Unlock()
			ws.Close()
			return
		}

		rest := append([]byte(nil), acc[h.Consumed:]...)
		acc = nil
		hdrDone = true
		finished = true
		mu.Unlock()
		deadline.Stop()

		if t.cfg.debug {
			t.logf("[Tunnel] %s cmd=%d → %s:%d head=%dB", h.Protocol, h.Command, h.Host, h.Port, len(rest))
		}
		if len(rest) > 0 && !q.Push(rest) {
			ws.Close()
			return
		}
		go t.run(ws, q, h)
	}
}

func (t *tunnel) run(ws *wsConn, q *frameQueue, h tunnelHeader) {
	isUDP := (h.Protocol == "vless" && h.Command == 2) || (h.Protocol == "trojan" && h.Command == 3)
	if isUDP {
		if h.Protocol != "vless" {
			ws.Close()
			return
		}
		t.relayUDP(ws, q, h)
		return
	}
	t.relayTCP(ws, q, h)
}

func (t *tunnel) relayTCP(ws *wsConn, q *frameQueue, h tunnelHeader) {
	if h.Protocol == "vless" {
		if err := ws.Send([]byte{h.Version, 0}); err != nil {
			ws.Close()
			return
		}
	}

	addr := net.JoinHostPort(h.Host, strconv.Itoa(h.Port))
	conn, err := net.DialTimeout("tcp", addr, 12*time.Second)
	if err != nil {
		ws.Close()
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	var once sync.Once
	shut := func() {
		once.Do(func() {
			conn.Close()
			q.Close()
			ws.Close()
		})
	}

	go func() {
		for {
			p, ok := q.Pop()
			if !ok {
				shut()
				return
			}
			if _, err := conn.Write(p); err != nil {
				shut()
				return
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if werr := ws.Send(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	shut()
}

func (t *tunnel) relayUDP(ws *wsConn, q *frameQueue, h tunnelHeader) {
	if t.cfg.vlessUDPHdr {
		if err := ws.Send([]byte{h.Version, 0}); err != nil {
			ws.Close()
			return
		}
	}

	family := "udp4"
	if ip := net.ParseIP(h.Host); ip != nil && ip.To4() == nil {
		family = "udp6"
	}
	raddr, err := net.ResolveUDPAddr(family, net.JoinHostPort(h.Host, strconv.Itoa(h.Port)))
	if err != nil {
		ws.Close()
		return
	}
	conn, err := net.DialUDP(family, nil, raddr)
	if err != nil {
		ws.Close()
		return
	}

	var once sync.Once
	shut := func() {
		once.Do(func() {
			conn.Close()
			q.Close()
			ws.Close()
		})
	}

	go func() {
		var pending []byte
		for {
			p, ok := q.Pop()
			if !ok {
				shut()
				return
			}
			if len(pending) == 0 {
				pending = p
			} else {
				pending = append(pending, p...)
			}
			for len(pending) >= 2 {
				n := int(binary.BigEndian.Uint16(pending[:2]))
				if len(pending) < n+2 {
					break
				}
				if _, err := conn.Write(pending[2 : 2+n]); err != nil {
					shut()
					return
				}
				pending = pending[2+n:]
			}
			if len(pending) > t.cfg.maxUDPBacklog {
				shut()
				return
			}
		}
	}()

	buf := make([]byte, 64*1024)
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			frame := make([]byte, 2+n)
			binary.BigEndian.PutUint16(frame[:2], uint16(n))
			copy(frame[2:], buf[:n])
			if werr := ws.Send(frame); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	shut()
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
