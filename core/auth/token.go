package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// NewToken returns a URL-safe token and its SHA-256 hash. Only the hash is stored.
// NewToken 生成 URL 安全令牌及其 SHA-256 哈希（只存哈希）。
func NewToken() (token string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken hashes a token for storage lookups.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
