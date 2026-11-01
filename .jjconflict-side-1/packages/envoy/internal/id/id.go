package id

import (
	"crypto/rand"
	"encoding/hex"
)

func New() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
