package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)


var (
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	pathRe   = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{8,96}$`)
	tokenRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	hostRe   = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)
	credsSec = "0600"
)

type Creds struct {
	File       string `json:"-"`
	UUID       string `json:"uuid"`
	SubToken   string `json:"subToken"`
	StreamPath string `json:"streamPath"`
	SubPath    string `json:"subPath"`
	CreatedAt  string `json:"createdAt"`

	UUIDHex    string `json:"-"`
	TrojanHash string `json:"-"`
	IsNew      bool   `json:"-"`
}

func genUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 理论上不会失败；真失败就用时间兜底，别让进程起不来
		t := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(t >> (8 * i))
			b[i+8] = byte(t >> (8 * (7 - i)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randPath(prefix string) string {
	return prefix + randToken(18)
}

func loadCreds(dir string, getenv func(string) string, logf func(string, ...any)) *Creds {
	file := filepath.Join(dir, ".creds.json")
	var stored Creds
	haveStored := false

	if raw := getenv("CREDS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &stored); err == nil {
			haveStored = true
		}
	}
	if !haveStored {
		if b, err := os.ReadFile(file); err == nil {
			if err := json.Unmarshal(b, &stored); err == nil {
				haveStored = true
			}
		}
	}

	next := stored
	changed := !haveStored
	if next.CreatedAt == "" {
		next.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}

	fix := func(field *string, valid func(string) bool, make_ func() string) {
		if !valid(*field) {
			*field = make_()
			changed = true
		}
	}
	fix(&next.UUID, func(v string) bool { return uuidRe.MatchString(v) }, genUUID)
	fix(&next.SubToken, func(v string) bool { return tokenRe.MatchString(v) }, func() string { return randToken(24) })
	fix(&next.StreamPath, func(v string) bool { return pathRe.MatchString(v) }, func() string { return randPath("/ws/") })
	fix(&next.SubPath, func(v string) bool { return pathRe.MatchString(v) }, func() string { return randPath("/s/") })

	var overrides []string
	if v := getenv("PROXY_UUID"); uuidRe.MatchString(v) {
		next.UUID = v
		overrides = append(overrides, "PROXY_UUID")
	}
	if v := getenv("SUB_TOKEN"); tokenRe.MatchString(v) {
		next.SubToken = v
		overrides = append(overrides, "SUB_TOKEN")
	}
	if v := getenv("STREAM_PATH"); pathRe.MatchString(v) {
		next.StreamPath = v
		overrides = append(overrides, "STREAM_PATH")
	}
	if v := getenv("SUB_PATH"); pathRe.MatchString(v) {
		next.SubPath = v
		overrides = append(overrides, "SUB_PATH")
	}

	next.File = file
	next.UUID = strings.ToLower(next.UUID)
	next.UUIDHex = strings.ReplaceAll(next.UUID, "-", "")
	next.TrojanHash = sha224Hex(next.UUID)
	next.IsNew = changed

	if changed {
		out := struct {
			UUID       string `json:"uuid"`
			SubToken   string `json:"subToken"`
			StreamPath string `json:"streamPath"`
			SubPath    string `json:"subPath"`
			CreatedAt  string `json:"createdAt"`
			UpdatedAt  string `json:"updatedAt"`
		}{next.UUID, next.SubToken, next.StreamPath, next.SubPath, next.CreatedAt, time.Now().UTC().Format(time.RFC3339Nano)}
		if b, err := json.MarshalIndent(out, "", "  "); err == nil {
			if err := os.WriteFile(file, b, 0o600); err == nil {
				_ = os.Chmod(file, 0o600)
				logf("[Tunnel] 已生成新凭证 → %s (%s)", file, credsSec)
			} else {
				logf("[Tunnel] 凭证落盘失败（本次仍可用，重启会换新）：%v", err)
			}
		}
	}
	if len(overrides) > 0 {
		logf("[Tunnel] 凭证被环境变量覆盖：%s", strings.Join(overrides, ", "))
	}
	return &next
}


type hdrStatus int

const (
	hdrNeed hdrStatus = iota
	hdrBad
	hdrOK
)

type tunnelHeader struct {
	Status   hdrStatus
	Protocol string
	Version  byte
	Command  byte
	Host     string
	Port     int
	Consumed int
}

func formatIPv6(b []byte, off int) string {
	ip := make(net.IP, 16)
	copy(ip, b[off:off+16])
	return ip.String()
}

func parseTunnelHeader(buf []byte, creds *Creds) tunnelHeader {
	if len(buf) == 0 {
		return tunnelHeader{Status: hdrNeed}
	}
	b0 := buf[0]
	plausible := (b0 >= '0' && b0 <= '9') || (b0 >= 'a' && b0 <= 'f') || (b0 >= 0x00 && b0 <= 0x0f)
	if !plausible {
		return tunnelHeader{Status: hdrBad}
	}

	if len(buf) >= 56 && strings.ToLower(string(buf[:56])) == creds.TrojanHash {
		if len(buf) < 60 {
			return tunnelHeader{Status: hdrNeed}
		}
		if buf[56] != 0x0d || buf[57] != 0x0a {
			return tunnelHeader{Status: hdrBad}
		}
		command := buf[58]
		i := 59
		at := buf[i]
		i++
		var host string
		switch at {
		case 1:
			if len(buf) < i+4 {
				return tunnelHeader{Status: hdrNeed}
			}
			host = net.IP(buf[i : i+4]).String()
			i += 4
		case 3:
			if len(buf) < i+1 {
				return tunnelHeader{Status: hdrNeed}
			}
			n := int(buf[i])
			i++
			if n == 0 {
				return tunnelHeader{Status: hdrBad}
			}
			if len(buf) < i+n {
				return tunnelHeader{Status: hdrNeed}
			}
			host = string(buf[i : i+n])
			i += n
		case 4:
			if len(buf) < i+16 {
				return tunnelHeader{Status: hdrNeed}
			}
			host = formatIPv6(buf, i)
			i += 16
		default:
			return tunnelHeader{Status: hdrBad}
		}
		if len(buf) < i+4 {
			return tunnelHeader{Status: hdrNeed}
		}
		port := int(buf[i])<<8 | int(buf[i+1])
		i += 2
		if buf[i] != 0x0d || buf[i+1] != 0x0a {
			return tunnelHeader{Status: hdrBad}
		}
		i += 2
		if host == "" || port == 0 {
			return tunnelHeader{Status: hdrBad}
		}
		return tunnelHeader{Status: hdrOK, Protocol: "trojan", Version: 0, Command: command, Host: host, Port: port, Consumed: i}
	}

	if len(buf) >= 17 && hex.EncodeToString(buf[1:17]) == creds.UUIDHex {
		if len(buf) < 19 {
			return tunnelHeader{Status: hdrNeed}
		}
		version := buf[0]
		addonLen := int(buf[17])
		i := 18 + addonLen
		if len(buf) < i+1 {
			return tunnelHeader{Status: hdrNeed}
		}
		command := buf[i]
		i++
		if command != 1 && command != 2 {
			return tunnelHeader{Status: hdrBad}
		}
		if len(buf) < i+2 {
			return tunnelHeader{Status: hdrNeed}
		}
		port := int(buf[i])<<8 | int(buf[i+1])
		i += 2
		if len(buf) < i+1 {
			return tunnelHeader{Status: hdrNeed}
		}
		at := buf[i]
		i++
		var host string
		switch at {
		case 1:
			if len(buf) < i+4 {
				return tunnelHeader{Status: hdrNeed}
			}
			host = net.IP(buf[i : i+4]).String()
			i += 4
		case 2:
			if len(buf) < i+1 {
				return tunnelHeader{Status: hdrNeed}
			}
			n := int(buf[i])
			i++
			if n == 0 {
				return tunnelHeader{Status: hdrBad}
			}
			if len(buf) < i+n {
				return tunnelHeader{Status: hdrNeed}
			}
			host = string(buf[i : i+n])
			i += n
		case 3:
			if len(buf) < i+16 {
				return tunnelHeader{Status: hdrNeed}
			}
			host = formatIPv6(buf, i)
			i += 16
		default:
			return tunnelHeader{Status: hdrBad}
		}
		if host == "" || port == 0 {
			return tunnelHeader{Status: hdrBad}
		}
		return tunnelHeader{Status: hdrOK, Protocol: "vless", Version: version, Command: command, Host: host, Port: port, Consumed: i}
	}

	return tunnelHeader{Status: hdrNeed}
}


func pickHost(r *http.Request, getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("PUBLIC_HOST")); v != "" && hostRe.MatchString(v) {
		return strings.ToLower(v)
	}
	raw := r.Header.Get("X-Forwarded-Host")
	if raw == "" {
		raw = r.Host
	}
	if i := strings.IndexByte(raw, ','); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	if i := strings.LastIndexByte(raw, ':'); i > 0 && !strings.Contains(raw[i:], "]") {
		raw = raw[:i]
	}
	if raw != "" && hostRe.MatchString(raw) {
		return raw
	}
	return "localhost"
}

func buildSubscription(creds *Creds, host string, port int, tls, sni, label, alpn string) (vless, trojan, raw, b64 string) {
	path := strings.ReplaceAll(strings.ReplaceAll(creds.StreamPath, "%", "%25"), "&", "%26")
	alpnPart := ""
	if alpn != "" {
		alpnPart = "&alpn=" + alpn
	}
	common := fmt.Sprintf("security=%s&sni=%s&type=ws&host=%s&path=%s%s", tls, sni, host, path, alpnPart)
	vless = fmt.Sprintf("vless://%s@%s:%d?encryption=none&%s&fp=chrome#%s-VLESS", creds.UUID, host, port, common, label)
	trojan = fmt.Sprintf("trojan://%s@%s:%d?%s&fp=chrome#%s-Trojan", creds.UUID, host, port, common, label)
	raw = vless + "\n" + trojan
	b64 = base64.StdEncoding.EncodeToString([]byte(raw))
	return
}

func safeEqual(a, b string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ── 环境变量小工具 ──────────────────────────────────────────────────────────
func envInt(getenv func(string) string, key string, def, min, max int) int {
	v, err := strconv.Atoi(strings.TrimSpace(getenv(key)))
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func envOn(getenv func(string) string, key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(getenv(key)))
	if v == "" {
		return def
	}
	return v != "off" && v != "0" && v != "false" && v != "no"
}
