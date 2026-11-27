package main

import (
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/jarvis.html assets/loading.html assets/en.json assets/snapshot.json
var assetsFS embed.FS

const barePage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
	`<title>SYS</title><style>body{margin:0;height:100vh;display:grid;place-items:center;background:#05080a;color:#6ee7c8;` +
	`font:13px/1.6 ui-monospace,monospace}div{text-align:center}span{color:#3f5b55}</style>` +
	`<div><div>SYS MONITOR</div><span>nominal · uptime service</span></div>`

type panelCfg struct {
	panel     bool // false = 连页面都不装，只回裸壳
	feedOn    bool
	refresh   time.Duration
	accessLog bool
	maxSSE    int
}

type sseHub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	max     int
}

func newSSEHub(max int) *sseHub {
	return &sseHub{clients: make(map[chan []byte]struct{}), max: max}
}

func (h *sseHub) add() (chan []byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= h.max {
		return nil, false
	}
	ch := make(chan []byte, 4)
	h.clients[ch] = struct{}{}
	return ch, true
}

func (h *sseHub) remove(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *sseHub) broadcast(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- frame:
		default: // 慢客户端本轮跳过，不在内存里堆数据
		}
	}
}

func (h *sseHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

type panel struct {
	creds     *Creds
	tun       *tunnel
	snap      *snapshotStore
	feeds     *feedRunner
	cfg       panelCfg
	getenv    func(string) string
	startedAt time.Time

	pageHTML    []byte
	loadingHTML []byte
	localeJSON  []byte
	sse         *sseHub
}

func loadPanelAssets(cfg panelCfg) (page, loading, locale []byte) {
	page = []byte(barePage)
	loading = []byte(barePage)
	if !cfg.panel {
		return
	}
	if b, err := assetsFS.ReadFile("assets/jarvis.html"); err == nil {
		page = injectLocale(b)
	}
	if b, err := assetsFS.ReadFile("assets/loading.html"); err == nil {
		loading = b
	}
	if b, err := assetsFS.ReadFile("assets/en.json"); err == nil {
		locale = b
	}
	return
}

func injectLocale(html []byte) []byte {
	b, err := assetsFS.ReadFile("assets/en.json")
	if err != nil {
		return html
	}
	safe := strings.ReplaceAll(string(b), "</script>", `<\/script>`)
	inject := []byte("<script>window.__CRUCIX_LOCALE__ = " + safe + ";</script></head>")
	return []byte(strings.Replace(string(html), "</head>", string(inject), 1))
}

func (p *panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	if method != http.MethodGet && method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if p.cfg.accessLog {
		p.feeds.logf("%s %s", method, r.URL.Path)
	}

	path := r.URL.Path

	// WS 隧道优先：路径必须精确等于 streamPath
	if path == p.creds.StreamPath && isWebsocketUpgrade(r) {
		p.tun.handleWS(w, r)
		return
	}
	if path == p.creds.SubPath {
		p.tun.handleSubscribe(w, r, p.getenv)
		return
	}

	switch path {
	case "/", "/index.html":
		writeBytes(w, 200, "text/html; charset=utf-8", p.pageHTML, "no-cache")
	case "/loading", "/loading.html":
		writeBytes(w, 200, "text/html; charset=utf-8", p.loadingHTML, "no-cache")
	case "/api/data":
		p.serveData(w, r)
	case "/api/health":
		p.serveHealth(w)
	case "/api/locales":
		writeBytes(w, 200, "application/json; charset=utf-8", []byte(`{"current":"en","supported":["en"]}`), "no-store")
	case "/events":
		p.serveSSE(w, r)
	case "/robots.txt":
		writeBytes(w, 200, "text/plain; charset=utf-8", []byte("User-agent: *\nDisallow: /\n"), "")
	default:
		p.serveStatic(w, path)
	}
}

func writeBytes(w http.ResponseWriter, code int, ctype string, body []byte, cache string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if cache != "" {
		w.Header().Set("Cache-Control", cache)
	}
	w.WriteHeader(code)
	w.Write(body)
}

func (p *panel) serveData(w http.ResponseWriter, r *http.Request) {
	buf, etag := p.snap.get()
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	writeBytes(w, 200, "application/json; charset=utf-8", buf, "public, max-age=60")
}

func (p *panel) serveHealth(w http.ResponseWriter) {
	meta := p.snap.metaCopy()
	st := p.feeds.state()
	body, _ := json.Marshal(map[string]any{
		"status":                "ok",
		"uptime":                int(time.Since(p.startedAt).Seconds()),
		"lastSweep":             meta["timestamp"],
		"nextSweep":             st.NextSweep,
		"sweepInProgress":       st.InProgress,
		"sweepStartedAt":        st.StartedAt,
		"sourcesOk":             meta["sourcesOk"],
		"sourcesFailed":         meta["sourcesFailed"],
		"sourcesQueried":        meta["sourcesQueried"],
		"feedEnabled":           p.cfg.feedOn,
		"refreshMinutes":        refreshMinutes(p.cfg),
		"lastError":             st.LastError,
		"llmEnabled":            false,
		"llmProvider":           nil,
		"telegramEnabled":       false,
		"refreshIntervalMinutes": nil,
		"language":              "en",
		"rss":                   rssMB(),
		"engine":                "go",
		"tunnelConns":           p.tun.connCount(),
		"clients":               p.sse.count(),
	})
	writeBytes(w, 200, "application/json; charset=utf-8", body, "no-store")
}

func refreshMinutes(cfg panelCfg) any {
	if !cfg.feedOn {
		return nil
	}
	return int(cfg.refresh.Minutes())
}

func (p *panel) serveSSE(w http.ResponseWriter, r *http.Request) {
	ch, ok := p.sse.add()
	if !ok {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	defer p.sse.remove(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	flusher, _ := w.(http.Flusher)
	io.WriteString(w, "retry: 300000\n\n")
	if flusher != nil {
		flusher.Flush()
	}

	keep := time.NewTicker(60 * time.Second)
	defer keep.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-keep.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

var mimeTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// serveStatic 只服务 PANEL_DIR 里明确指定过的文件；默认走内嵌资源，不暴露磁盘目录。
func (p *panel) serveStatic(w http.ResponseWriter, path string) {
	dir := p.getenv("PANEL_DIR")
	if dir == "" {
		http.Error(w, "Not Found", 404)
		return
	}
	rel := strings.TrimPrefix(filepath.Clean("/"+path), "/")
	if rel == "" || strings.Contains(rel, "..") {
		http.Error(w, "Not Found", 404)
		return
	}
	ext := strings.ToLower(filepath.Ext(rel))
	ctype, ok := mimeTypes[ext]
	if !ok {
		http.Error(w, "Not Found", 404)
		return
	}
	full := filepath.Join(dir, rel)
	b, err := os.ReadFile(full)
	if err != nil {
		http.Error(w, "Not Found", 404)
		return
	}
	writeBytes(w, 200, ctype, b, "public, max-age=300")
}

// rssMB 从 /proc/self/statm 读真实 RSS，和 Node 版的口径对得上。
func rssMB() int {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int(m.Sys / 1048576)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return int(pages * uint64(os.Getpagesize()) / 1048576)
}
