package main

import (
	"crypto/sha256"
	"encoding/hex"
)

func sha224Hex(s string) string {
	h := sha256.New224()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}
