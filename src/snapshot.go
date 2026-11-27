package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// 快照用 map[string]any 存放，和 JS 的 {...prev, ...next} 语义一致：
// 抓取只覆盖该覆盖的键，页面读的其它字段（metals/energy/各种块）原样保留。

type snapshotStore struct {
	mu   sync.RWMutex
	obj  map[string]any
	buf  []byte
	etag string
}

func newSnapshotStore() *snapshotStore {
	s := &snapshotStore{obj: map[string]any{}, buf: []byte(`{"error":"no snapshot"}`), etag: `W/"0-x"`}
	return s
}

func (s *snapshotStore) apply(obj map[string]any) {
	if obj == nil {
		return
	}
	buf, err := json.Marshal(obj)
	if err != nil {
		return
	}
	meta, _ := obj["meta"].(map[string]any)
	ts := "x"
	if meta != nil {
		if v, ok := meta["timestamp"].(string); ok && v != "" {
			ts = v
		}
	}
	s.mu.Lock()
	s.obj = obj
	s.buf = buf
	s.etag = `W/"` + itoa(len(buf)) + "-" + ts + `"`
	s.mu.Unlock()
}

func (s *snapshotStore) get() ([]byte, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buf, s.etag
}

func (s *snapshotStore) object() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.obj
}

func (s *snapshotStore) metaCopy() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]any{}
	if m, ok := s.obj["meta"].(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func readJSONFile(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil
	}
	return obj
}

func metaTime(obj map[string]any) time.Time {
	if obj == nil {
		return time.Time{}
	}
	meta, ok := obj["meta"].(map[string]any)
	if !ok {
		return time.Time{}
	}
	s, _ := meta["timestamp"].(string)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func itoa(n int) string {
	// 避免在热路径上引 strconv 的额外分支，这里够用
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func decodeJSON(b []byte) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil
	}
	return obj
}
