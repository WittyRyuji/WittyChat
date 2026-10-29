package auth

// AES-256-GCM sealing for small secrets (TOTP seeds).
// 小秘密（如 TOTP 种子）的 AES-256-GCM 加密。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

var errKeyLen = errors.New("secret key must decode to 32 bytes")

// LoadSecretKey reads WITTYCHAT_SECRET_KEY (base64, 32 bytes).
// Empty env → generates an ephemeral key and returns it with ok=false
// (dev only; restart loses sealed secrets — log a warning at call site).
//
// 从环境变量 WITTYCHAT_SECRET_KEY 读取 32 字节密钥（base64）。
// 未配置时生成临时密钥并返回 ok=false（仅开发用，重启后已加密内容不可解）。
func LoadSecretKey() (key []byte, ok bool, err error) {
	raw := os.Getenv("WITTYCHAT_SECRET_KEY")
	if raw == "" {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return nil, false, err
		}
		return k, false, nil
	}
	k, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, false, fmt.Errorf("decode WITTYCHAT_SECRET_KEY: %w", err)
	}
	if len(k) != 32 {
		return nil, false, errKeyLen
	}
	return k, true, nil
}

// SealSecret encrypts small secrets. Output = nonce || ciphertext.
func SealSecret(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// OpenSecret decrypts output of SealSecret.
func OpenSecret(key, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(sealed) <= n {
		return nil, errors.New("sealed secret too short")
	}
	return gcm.Open(nil, sealed[:n], sealed[n:], nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errKeyLen
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
