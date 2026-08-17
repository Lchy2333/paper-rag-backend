package service

import (
	"crypto/rand"
	"encoding/hex"
)

// newID 生成 32 位十六进制随机 ID。
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
