package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 只做服务端需要的 RFC6455 子集：握手、分片、ping/pong、close、背压。

const (
	opCont  byte = 0x0
	opText  byte = 0x1
	opBin   byte = 0x2
	opClose byte = 0x8
	opPing  byte = 0x9
	opPong  byte = 0xa
)

var errWSClosed = errors.New("websocket closed")

type wsConn struct {
	conn       net.Conn
	br         *bufio.Reader
	maxPayload int64

	wmu     sync.Mutex
	closed  atomic.Bool
	closeFn sync.Once

	onMessage func([]byte)
	onClose   func()

	// 写超时：对端不读的时候不能把整条连接的 goroutine 永久卡死
	writeWait time.Duration
}

func headerHasToken(h string, tok string) bool {
	for _, part := range strings.Split(h, ",") {
		if strings.EqualFold(strings.TrimSpace(part), tok) {
			return true
		}
	}
	return false
}

func isWebsocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header.Get("Connection"), "upgrade") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

// upgradeWS 完成握手并接管连接。失败时已经写了响应，调用方直接返回。
func upgradeWS(w http.ResponseWriter, r *http.Request, maxPayload int64) (*wsConn, error) {
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return nil, errors.New("bad ws handshake")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Not Supported", http.StatusInternalServerError)
		return nil, errors.New("hijack unsupported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h[:])

	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: ")
	b.WriteString(accept)
	b.WriteString("\r\n\r\n")
	if _, err := rw.WriteString(b.String()); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	return &wsConn{
		conn:       conn,
		br:         rw.Reader,
		maxPayload: maxPayload,
		writeWait:  60 * time.Second,
	}, nil
}

func (c *wsConn) start() {
	go c.readLoop()
}

func (c *wsConn) deliver(p []byte) {
	if c.onMessage != nil {
		c.onMessage(p)
	}
}

func (c *wsConn) readLoop() {
	var frag []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			c.Close()
			return
		}
		switch op {
		case opPing:
			_ = c.writeFrame(opPong, payload)
		case opPong:
			// 心跳回应，不需要处理
		case opClose:
			_ = c.writeFrame(opClose, payload)
			c.Close()
			return
		case opCont:
			if frag == nil {
				c.Close()
				return
			}
			if int64(len(frag))+int64(len(payload)) > c.maxPayload {
				c.Close()
				return
			}
			frag = append(frag, payload...)
			if fin {
				msg := frag
				frag = nil
				c.deliver(msg)
			}
		case opText, opBin:
			if fin {
				if int64(len(payload)) > c.maxPayload {
					c.Close()
					return
				}
				c.deliver(payload)
			} else {
				frag = append(frag[:0], payload...)
			}
		default:
			c.Close()
			return
		}
	}
}

func (c *wsConn) readFrame() (bool, byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin := h[0]&0x80 != 0
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := int64(h[1] & 0x7f)

	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		u := binary.BigEndian.Uint64(b[:])
		if u > uint64(1)<<40 {
			return false, 0, nil, errors.New("frame too large")
		}
		n = int64(u)
	}

	// 控制帧必须 <=125 且不能分片
	if op >= 0x8 && (n > 125 || !fin) {
		return false, 0, nil, errors.New("bad control frame")
	}
	if n < 0 || n > c.maxPayload {
		return false, 0, nil, errors.New("payload too large")
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return false, 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, op, payload, nil
}

func frameHeader(op byte, n int, fin bool) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	switch {
	case n < 126:
		return []byte{b0, byte(n)}
	case n <= 0xffff:
		h := make([]byte, 4)
		h[0] = b0
		h[1] = 126
		binary.BigEndian.PutUint16(h[2:], uint16(n))
		return h
	default:
		h := make([]byte, 10)
		h[0] = b0
		h[1] = 127
		binary.BigEndian.PutUint64(h[2:], uint64(n))
		return h
	}
}

func (c *wsConn) writeFrame(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed.Load() {
		return errWSClosed
	}
	if c.writeWait > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeWait))
	}
	if _, err := c.conn.Write(frameHeader(op, len(p), true)); err != nil {
		return err
	}
	if len(p) > 0 {
		if _, err := c.conn.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// Send 发一条二进制消息；超过 32KB 自动分片，整组分片在同一把锁里写完，
// 避免心跳帧插到分片序列中间。
func (c *wsConn) Send(p []byte) error {
	const chunk = 32 * 1024
	if len(p) <= chunk {
		return c.writeFrame(opBin, p)
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed.Load() {
		return errWSClosed
	}
	if c.writeWait > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeWait))
	}
	for off := 0; off < len(p); {
		end := off + chunk
		if end > len(p) {
			end = len(p)
		}
		op := opCont
		fin := end == len(p)
		if off == 0 {
			op = opBin
		}
		if _, err := c.conn.Write(frameHeader(op, end-off, fin)); err != nil {
			return err
		}
		if _, err := c.conn.Write(p[off:end]); err != nil {
			return err
		}
		off = end
	}
	return nil
}

func (c *wsConn) Ping() error {
	return c.writeFrame(opPing, nil)
}

func (c *wsConn) Close() {
	c.closeFn.Do(func() {
		c.closed.Store(true)
		_ = c.writeFrame(opClose, []byte{0x03, 0xe8})
		_ = c.conn.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
}

type frameQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	chunks [][]byte
	bytes  int
	max    int
	closed bool
}

func newFrameQueue(max int) *frameQueue {
	q := &frameQueue{max: max}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *frameQueue) Push(p []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	if q.bytes+len(p) > q.max {
		q.closed = true
		q.cond.Broadcast()
		return false
	}
	q.chunks = append(q.chunks, p)
	q.bytes += len(p)
	q.cond.Signal()
	return true
}

func (q *frameQueue) Pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.chunks) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.chunks) == 0 {
		return nil, false
	}
	p := q.chunks[0]
	q.chunks = q.chunks[1:]
	q.bytes -= len(p)
	return p, true
}

func (q *frameQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *frameQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes
}
