package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 动态数据：本意不是做全量情报，而是让面板上的数字真的会动。
// 约束和 Node 版一致：单源 256KB 硬顶、并发 2、抓完替换对象不留历史。

const (
	isoMillis = "2006-01-02T15:04:05.000Z07:00"
	ua         = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	maxBody    = 256 * 1024
	rssPerSrc  = 20
	newsLimit  = 40
	feedLimit  = 50
)

type rssSource struct {
	ID     string
	Name   string
	Region string
	URL    string
	Lat    float64
	Lon    float64
	hasGeo bool
}

var rssSources = []rssSource{
	{"bbc-world", "BBC World", "Global", "https://feeds.bbci.co.uk/news/world/rss.xml", 51.5, -0.12, true},
	{"aljazeera", "Al Jazeera", "Middle East", "https://www.aljazeera.com/xml/rss/all.xml", 25.28, 51.53, true},
	{"npr-world", "NPR World", "Americas", "https://feeds.npr.org/1004/rss.xml", 38.9, -77.03, true},
	{"dw-world", "DW", "Europe", "https://rss.dw.com/rdf/rss-en-world", 52.52, 13.4, true},
}

type newsItem struct {
	Title  string
	URL    string
	Date   string
	TS     int64
	Source string
	Region string
	Lat    float64
	Lon    float64
	hasGeo bool
}

var feedHTTP = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     60 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

func fetchText(ctx context.Context, url string, max int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := feedHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return "", err
	}
	if len(b) > max {
		return "", errors.New("body too large")
	}
	return string(b), nil
}

func postJSON(ctx context.Context, url string, payload any, max int) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := feedHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, errors.New("body too large")
	}
	return b, nil
}

// ── RSS ─────────────────────────────────────────────────────────────────────

var (
	tagStrip  = regexp.MustCompile(`(?s)<[^>]*>`)
	cdataRe   = regexp.MustCompile(`(?s)<!\[CDATA\[(.*?)\]\]>`)
	wsRe      = regexp.MustCompile(`\s+`)
	atomBlock = regexp.MustCompile(`(?si)<entry[\s>].*?</entry>`)
	itemBlock = regexp.MustCompile(`(?si)<(?:item|entry)[\s>].*?</(?:item|entry)>`)
)

func tagOf(block, name string) string {
	re := regexp.MustCompile(`(?si)<` + name + `(?:\s[^>]*)?>(.*?)</` + name + `>`)
	if m := re.FindStringSubmatch(block); m != nil {
		return m[1]
	}
	return ""
}

func cleanText(s string) string {
	s = cdataRe.ReplaceAllString(s, "$1")
	s = tagStrip.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

func linkOf(block string, atom bool) string {
	if atom {
		re := regexp.MustCompile(`(?i)<link[^>]*rel=["']alternate["'][^>]*href=["']([^"']+)["']`)
		if m := re.FindStringSubmatch(block); m != nil {
			return m[1]
		}
		re2 := regexp.MustCompile(`(?i)<link[^>]*href=["']([^"']+)["'][^>]*/?>`)
		if m := re2.FindStringSubmatch(block); m != nil {
			return m[1]
		}
	}
	return tagOf(block, "link")
}

func hashUnit(s string) float64 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return float64(h%1000) / 1000
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

var dateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05Z",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseDate(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

func parseFeed(xmlText string, src rssSource, limit int) []newsItem {
	atom := regexp.MustCompile(`(?i)<entry[\s>]`).MatchString(xmlText)
	blocks := itemBlock.FindAllString(xmlText, limit*2)
	out := make([]newsItem, 0, limit)

	for _, b := range blocks {
		if len(out) >= limit {
			break
		}
		title := cleanText(tagOf(b, "title"))
		if title == "" {
			continue
		}
		dateRaw := cleanText(tagOf(b, "pubDate"))
		if dateRaw == "" {
			dateRaw = cleanText(tagOf(b, "published"))
		}
		if dateRaw == "" {
			dateRaw = cleanText(tagOf(b, "updated"))
		}
		if dateRaw == "" {
			dateRaw = cleanText(tagOf(b, "dc:date"))
		}
		ts, ok := parseDate(dateRaw)
		dateStr := dateRaw
		if ok {
			dateStr = time.UnixMilli(ts).UTC().Format(time.RFC3339)
		}
		it := newsItem{
			Title:  title,
			URL:    cleanText(linkOf(b, atom)),
			Date:   dateStr,
			TS:     ts,
			Source: src.Name,
			Region: src.Region,
		}
		if src.hasGeo {
			j1 := hashUnit(title)
			j2 := hashUnit(src.Name + title)
			it.Lat = math.Round((src.Lat+(j1-0.5)*1.6)*1000) / 1000
			it.Lon = math.Round((src.Lon+(j2-0.5)*1.6)*1000) / 1000
			it.hasGeo = true
		}
		out = append(out, it)
	}
	return out
}

// ── 行情 ────────────────────────────────────────────────────────────────────

type quote struct {
	Price     float64
	ChangePct float64
	Closes    []float64
}

type tvSymbol struct {
	TV     string
	Symbol string
	Name   string
}

var tvMap = []tvSymbol{
	{"SP:SPX", "^GSPC", "S&P 500"},
	{"NASDAQ:NDX", "^IXIC", "Nasdaq 100"},
	{"DJ:DJI", "^DJI", "Dow Jones"},
	{"TVC:VIX", "^VIX", "VIX"},
	{"TVC:US10Y", "^TNX", "US 10Y"},
	{"TVC:US30Y", "^TYX", "US 30Y"},
	{"COMEX:GC1!", "GC=F", "Gold"},
	{"TVC:SILVER", "SI=F", "Silver"},
	{"NYMEX:CL1!", "CL=F", "WTI Crude"},
	{"BINANCE:BTCUSDT", "BTC", "Bitcoin"},
	{"BINANCE:ETHUSDT", "ETH", "Ethereum"},
}

func fetchTradingView(ctx context.Context) (map[string]quote, error) {
	tickers := make([]string, len(tvMap))
	for i, s := range tvMap {
		tickers[i] = s.TV
	}
	payload := map[string]any{
		"symbols": map[string]any{
			"tickers": tickers,
			"query":   map[string]any{"types": []string{}},
		},
		"columns": []string{"close", "change", "name"},
	}
	b, err := postJSON(ctx, "https://scanner.tradingview.com/global/scan", payload, 32768)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			S string `json:"s"`
			D []any  `json:"d"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	byTicker := make(map[string]tvSymbol, len(tvMap))
	for _, s := range tvMap {
		byTicker[s.TV] = s
	}
	out := make(map[string]quote, len(resp.Data))
	for _, row := range resp.Data {
		meta, ok := byTicker[row.S]
		if !ok || len(row.D) < 2 {
			continue
		}
		price, ok1 := toFloat(row.D[0])
		chg, ok2 := toFloat(row.D[1])
		if !ok1 || math.IsNaN(price) || price == 0 {
			continue
		}
		if !ok2 || math.IsNaN(chg) || math.IsInf(chg, 0) {
			chg = 0
		}
		prev := price
		if chg != 0 {
			prev = price / (1 + chg/100)
		}
		out[meta.Symbol] = quote{
			Price:     price,
			ChangePct: round2(chg),
			Closes:    []float64{round2(prev), round2(price)},
		}
	}
	if len(out) == 0 {
		return nil, errors.New("empty result")
	}
	return out, nil
}

func fetchYahooSpark(ctx context.Context, symbols []string) (map[string]quote, error) {
	hosts := []string{"https://query2.finance.yahoo.com", "https://query1.finance.yahoo.com"}
	qs := strings.Join(symbols, "%2C")
	var lastErr error = errors.New("no host")
	for _, h := range hosts {
		url := h + "/v8/finance/spark?symbols=" + qs + "&range=5d&interval=1d"
		txt, err := fetchText(ctx, url, 65536)
		if err != nil {
			lastErr = err
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(txt), &raw); err != nil {
			lastErr = err
			continue
		}
		out := map[string]quote{}
		for sym, msg := range raw {
			var v struct {
				Close             []*float64 `json:"close"`
				FulldayPrice      *float64   `json:"fulldayPrice"`
				FulldayChangePct  *float64   `json:"fulldayChangePercent"`
			}
			if json.Unmarshal(msg, &v) != nil {
				continue
			}
			closes := make([]float64, 0, len(v.Close))
			for _, c := range v.Close {
				if c != nil && !math.IsNaN(*c) {
					closes = append(closes, *c)
				}
			}
			var price float64
			if v.FulldayPrice != nil {
				price = *v.FulldayPrice
			} else if len(closes) > 0 {
				price = closes[len(closes)-1]
			} else {
				continue
			}
			pct := 0.0
			if len(closes) >= 2 && closes[len(closes)-2] > 0 {
				pct = (price - closes[len(closes)-2]) / closes[len(closes)-2] * 100
			} else if v.FulldayChangePct != nil {
				pct = *v.FulldayChangePct
			}
			if len(closes) > 24 {
				closes = closes[len(closes)-24:]
			}
			for i := range closes {
				closes[i] = round2(closes[i])
			}
			out[sym] = quote{Price: price, ChangePct: round2(pct), Closes: closes}
		}
		if len(out) > 0 {
			return out, nil
		}
		lastErr = errors.New("empty result")
	}
	return nil, lastErr
}

func fetchCoinGecko(ctx context.Context) ([]map[string]any, error) {
	txt, err := fetchText(ctx, "https://api.coingecko.com/api/v3/simple/price?ids=bitcoin,ethereum&vs_currencies=usd&include_24hr_change=true", 8192)
	if err != nil {
		return nil, err
	}
	var j map[string]map[string]float64
	if err := json.Unmarshal([]byte(txt), &j); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	if v, ok := j["bitcoin"]; ok && v["usd"] > 0 {
		out = append(out, map[string]any{"symbol": "BTC", "name": "Bitcoin", "price": v["usd"], "changePct": round2(v["usd_24h_change"])})
	}
	if v, ok := j["ethereum"]; ok && v["usd"] > 0 {
		out = append(out, map[string]any{"symbol": "ETH", "name": "Ethereum", "price": v["usd"], "changePct": round2(v["usd_24h_change"])})
	}
	return out, nil
}

type sweepStats struct {
	OK       int
	Failed   int
	MS       int64
	Failures []string
	News     int
	Quotes   int
}

func runSweep(base map[string]any, logf func(string, ...any)) (map[string]any, sweepStats) {
	t0 := time.Now()
	stats := sweepStats{}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// ── RSS：并发 2 ──
	type rssRes struct {
		src   rssSource
		items []newsItem
		err   error
	}
	resCh := make(chan rssRes, len(rssSources))
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for _, src := range rssSources {
		wg.Add(1)
		go func(src rssSource) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sctx, c := context.WithTimeout(ctx, 12*time.Second)
			defer c()
			xmlText, err := fetchText(sctx, src.URL, 131072)
			if err != nil {
				resCh <- rssRes{src: src, err: err}
				return
			}
			resCh <- rssRes{src: src, items: parseFeed(xmlText, src, rssPerSrc)}
		}(src)
	}
	wg.Wait()
	close(resCh)

	all := make([]newsItem, 0, 80)
	for r := range resCh {
		if r.err != nil {
			stats.Failed++
			stats.Failures = append(stats.Failures, r.src.ID+": "+r.err.Error())
			logf("[feed] %s 失败：%v", r.src.ID, r.err)
			continue
		}
		stats.OK++
		all = append(all, r.items...)
	}

	// 时间倒序 + 按标题去重
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].TS > all[j-1].TS; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	seen := map[string]bool{}
	unique := make([]newsItem, 0, feedLimit)
	for _, it := range all {
		key := strings.ToLower(truncRunes(it.Title, 80))
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, it)
		if len(unique) >= feedLimit {
			break
		}
	}

	news := make([]map[string]any, 0, newsLimit)
	newsFeed := make([]map[string]any, 0, len(unique))
	for i, it := range unique {
		if i < newsLimit {
			m := map[string]any{
				"title": it.Title, "source": it.Source, "date": it.Date,
				"url": it.URL, "region": it.Region,
			}
			if it.hasGeo {
				m["lat"] = it.Lat
				m["lon"] = it.Lon
			}
			news = append(news, m)
		}
		n := map[string]any{
			"headline": it.Title, "source": it.Source, "type": "rss",
			"timestamp": it.Date, "region": it.Region, "urgent": false, "url": it.URL,
		}
		if it.hasGeo {
			n["lat"] = it.Lat
			n["lon"] = it.Lon
		}
		newsFeed = append(newsFeed, n)
	}

	// ── 行情：TradingView 主，Yahoo 备 ──
	q := map[string]quote{}
	if tvq, err := fetchTradingView(ctx); err == nil {
		q = tvq
		stats.OK++
	} else {
		stats.Failed++
		stats.Failures = append(stats.Failures, "tradingview: "+err.Error())
		logf("[feed] tradingview 失败：%v", err)
	}
	if len(q) == 0 {
		syms := []string{"^GSPC", "^IXIC", "^DJI", "^TNX", "^TYX", "GC=F", "CL=F", "^VIX"}
		if yq, err := fetchYahooSpark(ctx, syms); err == nil {
			q = yq
			stats.OK++
		} else {
			stats.Failed++
			stats.Failures = append(stats.Failures, "yahoo: "+err.Error())
			logf("[feed] yahoo 失败：%v", err)
		}
	}

	prevMarkets := map[string]any{}
	if pm, ok := base["markets"].(map[string]any); ok {
		prevMarkets = pm
	}
	mk := func(sym, name string) map[string]any {
		v, ok := q[sym]
		if !ok {
			return nil
		}
		return map[string]any{"symbol": sym, "name": name, "price": v.Price, "changePct": v.ChangePct}
	}
	indexes := pickQuotes([]map[string]any{mk("^GSPC", "S&P 500"), mk("^IXIC", "Nasdaq 100"), mk("^DJI", "Dow Jones")})
	rates := pickQuotes([]map[string]any{mk("^TNX", "US 10Y"), mk("^TYX", "US 30Y")})
	cr := pickQuotes([]map[string]any{mk("BTC", "Bitcoin"), mk("ETH", "Ethereum")})
	if len(cr) == 0 {
		if cg, err := fetchCoinGecko(ctx); err == nil && len(cg) > 0 {
			cr = cg
			stats.OK++
		} else if err != nil {
			stats.Failed++
			stats.Failures = append(stats.Failures, "coingecko: "+err.Error())
		}
	}

	markets := map[string]any{}
	for k, v := range prevMarkets {
		markets[k] = v
	}
	if len(indexes) > 0 {
		markets["indexes"] = indexes
	}
	if len(cr) > 0 {
		markets["crypto"] = cr
	}
	if len(rates) > 0 {
		markets["rates"] = rates
	}
	if vix, ok := q["^VIX"]; ok {
		markets["vix"] = map[string]any{"value": vix.Price, "changePct": vix.ChangePct}
	}
	markets["timestamp"] = time.Now().UTC().Format(isoMillis)

	next := map[string]any{}
	for k, v := range base {
		next[k] = v
	}
	next["markets"] = markets

	// 贵金属 / 原油：顶栏那几个数字跟着动
	if metals, ok := base["metals"].(map[string]any); ok {
		m := map[string]any{}
		for k, v := range metals {
			m[k] = v
		}
		if g, ok := q["GC=F"]; ok {
			old, _ := toFloat(m["gold"])
			if old == 0 {
				old = g.Price
			}
			m["gold"] = g.Price
			m["goldChangePct"] = g.ChangePct
			m["goldChange"] = round2(g.Price - old)
			if len(g.Closes) > 0 {
				m["goldRecent"] = g.Closes
			} else {
				m["goldRecent"] = []float64{g.Price}
			}
		}
		if s, ok := q["SI=F"]; ok {
			old, _ := toFloat(m["silver"])
			if old == 0 {
				old = s.Price
			}
			m["silver"] = s.Price
			m["silverChangePct"] = s.ChangePct
			m["silverChange"] = round2(s.Price - old)
			if len(s.Closes) > 0 {
				m["silverRecent"] = s.Closes
			} else {
				m["silverRecent"] = []float64{s.Price}
			}
		}
		next["metals"] = m
	}
	if energy, ok := base["energy"].(map[string]any); ok {
		e := map[string]any{}
		for k, v := range energy {
			e[k] = v
		}
		if w, ok := q["CL=F"]; ok {
			e["wti"] = w.Price
			if len(w.Closes) > 0 {
				e["wtiRecent"] = w.Closes
			} else {
				e["wtiRecent"] = []float64{w.Price}
			}
		}
		next["energy"] = e
	}

	if len(news) > 0 {
		next["news"] = news
	}
	if len(newsFeed) > 0 {
		next["newsFeed"] = newsFeed
	}

	meta := map[string]any{}
	if pm, ok := base["meta"].(map[string]any); ok {
		for k, v := range pm {
			meta[k] = v
		}
	}
	ms := time.Since(t0).Milliseconds()
	meta["timestamp"] = time.Now().UTC().Format(isoMillis)
	meta["totalDurationMs"] = ms
	meta["sweepMs"] = ms
	meta["sourcesOk"] = stats.OK
	meta["sourcesFailed"] = stats.Failed
	meta["sourcesQueried"] = len(rssSources) + 2
	next["meta"] = meta

	stats.MS = ms
	stats.News = len(news)
	stats.Quotes = len(q)
	return next, stats
}

func pickQuotes(in []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, m := range in {
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}

func toFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ── 定时器 ──────────────────────────────────────────────────────────────────

type feedState struct {
	InProgress bool
	StartedAt  string
	NextSweep  string
	LastError  any
}

type feedRunner struct {
	mu        sync.Mutex
	st        feedState
	snap      *snapshotStore
	hub       *sseHub
	every     time.Duration
	enabled   bool
	cacheFile string
	cacheOn   bool
	logf      func(string, ...any)
	stop      chan struct{}
	once      sync.Once
}

func (f *feedRunner) state() feedState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *feedRunner) setState(fn func(*feedState)) {
	f.mu.Lock()
	fn(&f.st)
	f.mu.Unlock()
}

func (f *feedRunner) loop() {
	if !f.enabled {
		return
	}
	f.logf("[feed] 动态数据已开启，每 %d 分钟抓一次", int(f.every.Minutes()))
	first := time.NewTimer(1500 * time.Millisecond)
	select {
	case <-first.C:
	case <-f.stop:
		return
	}
	reason := "boot"
	for {
		f.sweep(reason)
		reason = "timer"
		f.setState(func(s *feedState) {
			s.NextSweep = time.Now().Add(f.every).UTC().Format(isoMillis)
		})
		t := time.NewTimer(f.every)
		select {
		case <-t.C:
		case <-f.stop:
			t.Stop()
			return
		}
	}
}

func (f *feedRunner) sweep(reason string) {
	if !f.enabled {
		return
	}
	f.setState(func(s *feedState) {
		if s.InProgress {
			return
		}
		s.InProgress = true
		s.StartedAt = time.Now().UTC().Format(isoMillis)
	})
	if st := f.state(); !st.InProgress {
		return
	}
	f.hub.broadcast([]byte("data: {\"type\":\"sweep_start\"}\n\n"))

	base := f.snap.object()
	snap, stats := runSweep(base, f.logf)
	f.snap.apply(snap)

	buf, _ := f.snap.get()
	frame := make([]byte, 0, len(buf)+48)
	frame = append(frame, []byte("data: {\"type\":\"update\",\"data\":")...)
	frame = append(frame, buf...)
	frame = append(frame, []byte("}\n\n")...)
	f.hub.broadcast(frame)

	var lastErr any = nil
	if len(stats.Failures) > 0 {
		lastErr = stats.Failures[0]
	}
	f.setState(func(s *feedState) {
		s.InProgress = false
		s.LastError = lastErr
	})

	f.logf("[feed] %s ok=%d fail=%d %dms news=%d rss=%dMB", reason, stats.OK, stats.Failed, stats.MS, stats.News, rssMB())
	f.persist()
}

func (f *feedRunner) persist() {
	if !f.cacheOn || f.cacheFile == "" {
		return
	}
	buf, _ := f.snap.get()
	if err := os.WriteFile(f.cacheFile, buf, 0o644); err != nil {
		f.logf("[feed] 快照缓存写入失败（只影响重启速度）：%v", err)
	}
}
