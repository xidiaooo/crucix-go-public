package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var logf = func(format string, args ...any) {
	log.Printf(format, args...)
}

func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

// resolveRunDir：项目目录能写就用 ./runs，只读文件系统（不少 PaaS）退到临时目录。
func resolveRunDir(getenv func(string) string) (string, bool) {
	if v := getenv("RUNS_DIR"); v != "" {
		if err := os.MkdirAll(v, 0o755); err == nil {
			return v, true
		}
	}
	dir := "runs"
	if err := os.MkdirAll(dir, 0o755); err == nil {
		probe := filepath.Join(dir, ".w")
		if err := os.WriteFile(probe, []byte(""), 0o644); err == nil {
			os.Remove(probe)
			return dir, true
		}
	}
	tmp := filepath.Join(os.TempDir(), "crucix-runs")
	_ = os.MkdirAll(tmp, 0o755)
	return tmp, false
}

func main() {
	loadDotEnv(".env")

	getenv := os.Getenv

	mask := strings.TrimSpace(getenv("PROC_MASK"))
	if mask == "" {
		mask = "node server.js"
	}
	if !strings.EqualFold(mask, "off") {
		if os.Getenv("_CM_MASKED") != "1" {
			maybeReexec(mask)
			applyProcMask(mask)
		} else {
			setComm(mask)
		}
	}

	// 内存上限：Go 默认会吃到容器一半内存，这里主动压低。
	if v := envInt(getenv, "MEM_LIMIT_MB", 96, 32, 4096); v > 0 {
		debug.SetMemoryLimit(int64(v) * 1024 * 1024)
	}
	if v := envInt(getenv, "GOGC_PERCENT", 50, 10, 400); v > 0 {
		debug.SetGCPercent(v)
	}

	runDir, writable := resolveRunDir(getenv)
	if !writable {
		logf("[Crucix] 项目目录不可写，运行数据改放临时目录（重启后凭证会重新生成）")
	}

	port := envInt(getenv, "PORT", 3117, 1, 65535)
	bind := getenv("BIND_HOST")
	if bind == "" {
		bind = "0.0.0.0"
	}

	cfg := panelCfg{
		panel:     envOn(getenv, "PANEL", true),
		feedOn:    envOn(getenv, "FEED", true),
		refresh:   time.Duration(envInt(getenv, "REFRESH_MINUTES", 10, 1, 1440)) * time.Minute,
		accessLog: envOn(getenv, "ACCESS_LOG", false),
		maxSSE:    envInt(getenv, "MAX_SSE_CLIENTS", 32, 1, 512),
	}

	store := newSnapshotStore()

	// 快照：内置 → 本地缓存（更新的那份）→ 之后由抓取替换
	snapPath := getenv("SNAPSHOT_FILE")
	if snapPath == "" {
		snapPath = "assets/snapshot.json"
	}
	baseSnap := readJSONFile(snapPath)
	if baseSnap == nil {
		if b, err := assetsFS.ReadFile("assets/snapshot.json"); err == nil {
			baseSnap = decodeJSON(b)
		}
	}
	if baseSnap == nil {
		logf("[Crucix] 内置快照读取失败（面板会显示空数据）")
		baseSnap = map[string]any{}
	}
	cacheFile := getenv("CACHE_FILE")
	if cacheFile == "" {
		cacheFile = filepath.Join(runDir, "snapshot.cache.json")
	}
	if cached := readJSONFile(cacheFile); cached != nil {
		if metaTime(cached).After(metaTime(baseSnap)) {
			baseSnap = cached
		}
	}
	store.apply(baseSnap)

	creds := loadCreds(runDir, getenv, logf)

	tcfg := tunnelCfg{
		maxConns:      envInt(getenv, "MAX_TUNNEL_CONNS", 64, 1, 4096),
		maxHeader:     envInt(getenv, "MAX_HEADER_BYTES", 65536, 1024, 1048576),
		handshake:     time.Duration(envInt(getenv, "HANDSHAKE_TIMEOUT_MS", 10000, 1000, 60000)) * time.Millisecond,
		maxWSBacklog:  envInt(getenv, "MAX_WS_BACKLOG_BYTES", 16*1024*1024, 65536, 256*1024*1024),
		maxUDPBacklog: envInt(getenv, "MAX_UDP_BACKLOG_BYTES", 1024*1024, 65536, 16*1024*1024),
		vlessUDPHdr:   envOn(getenv, "VLESS_UDP_HEADER", true),
		heartbeat:     time.Duration(envInt(getenv, "WS_HEARTBEAT_SECONDS", 25, 10, 300)) * time.Second,
		idle:          time.Duration(envInt(getenv, "TUNNEL_IDLE_MINUTES", 30, 0, 1440)) * time.Minute,
		maxPayload:    int64(envInt(getenv, "MAX_WS_PAYLOAD_BYTES", 2*1024*1024, 16384, 64*1024*1024)),
		debug:         envOn(getenv, "TUNNEL_DEBUG", false),
	}
	tun := newTunnel(creds, tcfg, logf)

	hub := newSSEHub(cfg.maxSSE)
	page, loading, locale := loadPanelAssets(cfg)

	feeds := &feedRunner{
		snap:      store,
		hub:       hub,
		every:     cfg.refresh,
		enabled:   cfg.feedOn,
		cacheFile: cacheFile,
		cacheOn:   envOn(getenv, "CACHE", true),
		logf:      logf,
		stop:      make(chan struct{}),
	}

	p := &panel{
		creds:       creds,
		tun:         tun,
		snap:        store,
		feeds:       feeds,
		cfg:         cfg,
		getenv:      getenv,
		startedAt:   time.Now(),
		pageHTML:    page,
		loadingHTML: loading,
		localeJSON:  locale,
		sse:         hub,
	}

	srv := &http.Server{
		Addr:              bind + ":" + strconv.Itoa(port),
		Handler:           p,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       75 * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}

	go feeds.loop()
	go memReclaim(tun, cfg.feedOn)

	// 启动 5 分钟后删除落盘的凭证文件（内存中的凭证不受影响）。
	go func() {
		time.Sleep(5 * time.Minute)
		if creds.File != "" {
			if err := os.Remove(creds.File); err == nil {
				logf("[Crucix] 凭证文件已清除")
			}
		}
	}()

	go func() {
		logf("[Crucix] listening on %s:%d | panel=%s | feed=%s | engine=go | tunnel ready",
			bind, port, onOff(cfg.panel), onOff(cfg.feedOn))
		if creds.IsNew {
			logf("[Tunnel] 首次启动，已生成独立凭证 → 访问 /%s 或 /api/health 查看状态", strings.TrimPrefix(creds.SubPath, "/"))
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("[Crucix] server error: %v", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	logf("[Crucix] 收到退出信号 → 收尾")
	close(feeds.stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func memReclaim(tun *tunnel, feedOn bool) {
	tick := time.NewTicker(2 * time.Minute)
	defer tick.Stop()
	for range tick.C {
		if tun.connCount() > 0 {
			continue
		}
		if rssMB() < 32 {
			continue
		}
		debug.FreeOSMemory()
		logf("[Crucix] 空闲归还内存 → %dMB", rssMB())
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
