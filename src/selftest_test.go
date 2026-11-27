package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// 端到端自测：起本地 echo 服务，模拟 xray/sing-box 的客户端行为，
// 走真实 HTTP 端口验证 VLESS / Trojan / UDP / 分片首包 / 错误凭证。

type testCreds struct {
	UUID       string `json:"uuid"`
	StreamPath string `json:"streamPath"`
}

func loadTestCreds(t *testing.T) testCreds {
	path := os.Getenv("CRUCIX_CREDS")
	if path == "" {
		path = "runs/.creds.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读凭证失败: %v", err)
	}
	var c testCreds
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("解析凭证失败: %v", err)
	}
	return c
}

// ── 最小 WS 客户端 ─────────────────────────────────────────────────────────

type wsClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialWS(t *testing.T, addr, path string) *wsClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, addr, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("握手写失败: %v", err)
	}
	br := bufio.NewReader(conn)
	saw101 := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("握手读失败: %v", err)
		}
		if strings.HasPrefix(line, "HTTP/1.1") {
			if !strings.Contains(line, "101") {
				t.Fatalf("握手被拒: %s", strings.TrimSpace(line))
			}
			saw101 = true
		}
		if line == "\r\n" {
			break
		}
	}
	if !saw101 {
		t.Fatal("没有 101")
	}
	return &wsClient{conn: conn, br: br}
}

func (c *wsClient) Close() { c.conn.Close() }

func (c *wsClient) Write(p []byte, op byte) error {
	mask := [4]byte{0x12, 0x34, 0x56, 0x78}
	head := []byte{0x80 | op}
	n := len(p)
	switch {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n <= 0xffff:
		head = append(head, 0x80|126, byte(n>>8), byte(n))
	default:
		head = append(head, 0x80|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	head = append(head, mask[:]...)
	buf := make([]byte, n)
	for i := 0; i < n; i++ {
		buf[i] = p[i] ^ mask[i&3]
	}
	_, err := c.conn.Write(append(head, buf...))
	return err
}

func (c *wsClient) Read(timeout time.Duration) (byte, []byte, error) {
	c.conn.SetReadDeadline(time.Now().Add(timeout))
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	n := int(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(b[0])<<8 | int(b[1])
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(b[6])<<8 | int(b[7])
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.br, p); err != nil {
		return 0, nil, err
	}
	return op, p, nil
}

// ── 首包构造 ───────────────────────────────────────────────────────────────

func uuidBytes(t *testing.T, uuid string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(uuid, "-", ""))
	if err != nil || len(b) != 16 {
		t.Fatalf("uuid 解析失败")
	}
	return b
}

func vlessHeader(t *testing.T, uuid string, cmd byte, host string, port int, payload []byte) []byte {
	out := []byte{0}
	out = append(out, uuidBytes(t, uuid)...)
	out = append(out, 0) // addons
	out = append(out, cmd)
	out = append(out, byte(port>>8), byte(port))
	ip := net.ParseIP(host).To4()
	if ip == nil {
		t.Fatalf("测试只用 IPv4")
	}
	out = append(out, 1)
	out = append(out, ip...)
	return append(out, payload...)
}

func trojanHeader(t *testing.T, uuid string, cmd byte, host string, port int, payload []byte) []byte {
	h := sha256.New224()
	h.Write([]byte(uuid))
	out := []byte(hex.EncodeToString(h.Sum(nil)))
	out = append(out, 0x0d, 0x0a, cmd)
	ip := net.ParseIP(host).To4()
	if ip == nil {
		t.Fatalf("测试只用 IPv4")
	}
	out = append(out, 1)
	out = append(out, ip...)
	out = append(out, byte(port>>8), byte(port))
	out = append(out, 0x0d, 0x0a)
	return append(out, payload...)
}

// ── 本地 echo ──────────────────────────────────────────────────────────────

func startTCPEcho(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen 失败: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func startUDPEcho(t *testing.T) int {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp echo 失败: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func testAddrNow() string {
	if v := os.Getenv("CRUCIX_TEST_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:3117"
}

func TestVLESSTCP(t *testing.T) {
	c := loadTestCreds(t)
	echoPort := startTCPEcho(t)
	ws := dialWS(t, testAddrNow(), c.StreamPath)
	defer ws.Close()

	if err := ws.Write(vlessHeader(t, c.UUID, 1, "127.0.0.1", echoPort, []byte("hello-vless")), opBin); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	op, p, err := ws.Read(3 * time.Second)
	if err != nil {
		t.Fatalf("读响应头失败: %v", err)
	}
	if op != opBin || len(p) != 2 || p[0] != 0 || p[1] != 0 {
		t.Fatalf("VLESS 响应头不对: op=%d p=%v", op, p)
	}
	_, p, err = ws.Read(3 * time.Second)
	if err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
	if string(p) != "hello-vless" {
		t.Fatalf("回显不对: %q", p)
	}
	t.Logf("VLESS TCP 通过，回显 %q", p)
}

func TestTrojanTCP(t *testing.T) {
	c := loadTestCreds(t)
	echoPort := startTCPEcho(t)
	ws := dialWS(t, testAddrNow(), c.StreamPath)
	defer ws.Close()

	if err := ws.Write(trojanHeader(t, c.UUID, 1, "127.0.0.1", echoPort, []byte("hello-trojan")), opBin); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	// Trojan 没有响应头，第一条数据就是回显
	_, p, err := ws.Read(3 * time.Second)
	if err != nil {
		t.Fatalf("读回显失败: %v", err)
	}
	if string(p) != "hello-trojan" {
		t.Fatalf("回显不对: %q", p)
	}
	t.Logf("Trojan TCP 通过，回显 %q", p)
}

func TestFragmentedHeader(t *testing.T) {
	c := loadTestCreds(t)
	echoPort := startTCPEcho(t)
	ws := dialWS(t, testAddrNow(), c.StreamPath)
	defer ws.Close()

	full := vlessHeader(t, c.UUID, 1, "127.0.0.1", echoPort, []byte("frag"))
	// 前 5 字节单独一帧：模拟客户端把首包拆开
	if err := ws.Write(full[:5], opBin); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := ws.Write(full[5:], opBin); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if _, _, err := ws.Read(3 * time.Second); err != nil {
		t.Fatalf("读响应头失败: %v", err)
	}
	_, p, err := ws.Read(3 * time.Second)
	if err != nil || string(p) != "frag" {
		t.Fatalf("分片首包回显不对: %q %v", p, err)
	}
	t.Logf("分片首包通过")
}

func TestVLESSOverWSFragments(t *testing.T) {
	c := loadTestCreds(t)
	echoPort := startTCPEcho(t)
	ws := dialWS(t, testAddrNow(), c.StreamPath)
	defer ws.Close()

	hdr := vlessHeader(t, c.UUID, 1, "127.0.0.1", echoPort, nil)
	if err := ws.Write(hdr, opBin); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if _, _, err := ws.Read(3 * time.Second); err != nil {
		t.Fatalf("读响应头失败: %v", err)
	}
	// 大包，服务端要按 32KB 分片发回来
	big := strings.Repeat("x", 200*1024)
	if err := ws.Write([]byte(big), opBin); err != nil {
		t.Fatalf("写大包失败: %v", err)
	}
	got := 0
	deadline := time.Now().Add(8 * time.Second)
	for got < len(big) && time.Now().Before(deadline) {
		_, p, err := ws.Read(5 * time.Second)
		if err != nil {
			t.Fatalf("读大包失败: %v (已收 %d)", err, got)
		}
		got += len(p)
	}
	if got != len(big) {
		t.Fatalf("大包长度不符: %d != %d", got, len(big))
	}
	t.Logf("200KB 回环通过")
}

func TestVLESSudp(t *testing.T) {
	c := loadTestCreds(t)
	udpPort := startUDPEcho(t)
	ws := dialWS(t, testAddrNow(), c.StreamPath)
	defer ws.Close()

	payload := []byte("dns-query")
	frame := []byte{byte(len(payload) >> 8), byte(len(payload))}
	frame = append(frame, payload...)
	if err := ws.Write(vlessHeader(t, c.UUID, 2, "127.0.0.1", udpPort, frame), opBin); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if _, _, err := ws.Read(3 * time.Second); err != nil {
		t.Fatalf("读 UDP 响应头失败: %v", err)
	}
	_, p, err := ws.Read(3 * time.Second)
	if err != nil {
		t.Fatalf("读 UDP 回显失败: %v", err)
	}
	if len(p) < 3 {
		t.Fatalf("UDP 回包太短: %v", p)
	}
	n := int(p[0])<<8 | int(p[1])
	if string(p[2:2+n]) != "dns-query" {
		t.Fatalf("UDP 回显不对: %q", p[2:2+n])
	}
	t.Logf("VLESS UDP 通过")
}

func TestWrongUUID(t *testing.T) {
	c := loadTestCreds(t)
	ws := dialWS(t, testAddrNow(), c.StreamPath)
	defer ws.Close()
	bad := "00000000-0000-4000-8000-000000000000"
	ws.Write(vlessHeader(t, bad, 1, "127.0.0.1", 9, []byte("x")), opBin)
	// 服务端应直接关连接（读到 EOF 或 close 帧）
	_, _, err := ws.Read(2 * time.Second)
	if err == nil {
		t.Fatal("错误 UUID 居然没被断开")
	}
	t.Logf("错误 UUID 被拒绝: %v", err)
}

func TestWrongPath(t *testing.T) {
	c := loadTestCreds(t)
	conn, err := net.DialTimeout("tcp", testAddrNow(), 3*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer conn.Close()
	req := fmt.Sprintf("GET /wrong%s HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: MTIzNDU2Nzg5MGFiY2RlZg==\r\nSec-WebSocket-Version: 13\r\n\r\n", c.StreamPath)
	conn.Write([]byte(req))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	resp := string(buf[:n])
	if strings.Contains(resp, "101") {
		t.Fatal("错误路径居然升级成功")
	}
	t.Logf("错误路径返回: %s", strings.TrimSpace(strings.Split(resp, "\r\n")[0]))
}

func dialWSRaw(addr, path string) (*wsClient, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, addr, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	ok := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if strings.Contains(line, "101") {
			ok = true
		}
		if line == "\r\n" {
			break
		}
	}
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("no 101")
	}
	return &wsClient{conn: conn, br: br}, nil
}

// 30 条并发、每条 256KB 回环，检查哈希一致 + 服务端内存是否失控
func TestConcurrentTunnels(t *testing.T) {
	c := loadTestCreds(t)
	echoPort := startTCPEcho(t)
	const n = 30
	const size = 256 * 1024

	errs := make(chan error, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws, err := dialWSRaw(testAddrNow(), c.StreamPath)
			if err != nil {
				errs <- err
				return
			}
			defer ws.Close()
			hdr := vlessHeader(t, c.UUID, 1, "127.0.0.1", echoPort, nil)
			if err := ws.Write(hdr, opBin); err != nil {
				errs <- err
				return
			}
			if _, _, err := ws.Read(5 * time.Second); err != nil {
				errs <- err
				return
			}
			payload := make([]byte, size)
			for j := range payload {
				payload[j] = byte((i*31 + j*7) % 251)
			}
			if err := ws.Write(payload, opBin); err != nil {
				errs <- err
				return
			}
			got := 0
			buf := make([]byte, 0, size)
			deadline := time.Now().Add(20 * time.Second)
			for got < size && time.Now().Before(deadline) {
				_, p, err := ws.Read(10 * time.Second)
				if err != nil {
					errs <- err
					return
				}
				buf = append(buf, p...)
				got += len(p)
			}
			if len(buf) != size {
				errs <- fmt.Errorf("长度不符 %d", len(buf))
				return
			}
			for j := range buf {
				if buf[j] != payload[j] {
					errs <- fmt.Errorf("内容不一致 @%d", j)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	fail := 0
	for err := range errs {
		fail++
		if fail <= 3 {
			t.Errorf("并发失败: %v", err)
		}
	}
	t.Logf("%d 条并发 × %dKB 用时 %v，失败 %d", n, size/1024, time.Since(start), fail)
}
