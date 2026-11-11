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
	"path/filepath"
	"strings"
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

// LoadOrCreateSecretKey resolves the workspace secret key, in order:
//  1. WITTYCHAT_SECRET_KEY env (unchanged behaviour / 保持环境变量优先)
//  2. dataDir/secret.key (base64, 0600) — created on first boot and reused
//     across restarts so sealed secrets (TOTP seeds…) survive.
//
// Returns (key, source, err); source ∈ {"env","file","created"}.
// 密钥落盘持久化：env > data/secret.key（0600，首次启动生成），重启不丢加密数据。
func LoadOrCreateSecretKey(dataDir string) (key []byte, source string, err error) {
	if raw := os.Getenv("WITTYCHAT_SECRET_KEY"); raw != "" {
		k, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, "", fmt.Errorf("decode WITTYCHAT_SECRET_KEY: %w", err)
		}
		if len(k) != 32 {
			return nil, "", errKeyLen
		}
		return k, "env", nil
	}

	p := filepath.Join(dataDir, "secret.key")
	if raw, err := os.ReadFile(p); err == nil { //nolint:gosec // fixed path under data dir
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(k) != 32 {
			return nil, "", fmt.Errorf("corrupt %s — restore from backup or delete to regenerate", p)
		}
		return k, "file", nil
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, "", fmt.Errorf("write %s: %w", p, err)
	}
	return k, "created", nil
}

// LoadSecretKeyFile reads dataDir/secret.key (base64, 32 bytes) — the
// load-only variant for processes (worker) that must NOT create one: a
// worker-generated key would differ from the server's and silently break
// sealed secrets. Errors when the file is missing or corrupt.
// 只读加载 data/secret.key——worker 等进程不许自建（自建密钥与服务端不一致，
// 密文全部解密失败）。文件缺失/损坏即报错。
func LoadSecretKeyFile(dataDir string) ([]byte, error) {
	p := filepath.Join(dataDir, "secret.key")
	raw, err := os.ReadFile(p) //nolint:gosec // fixed path under data dir
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(k) != 32 {
		return nil, fmt.Errorf("corrupt %s", p)
	}
	return k, nil
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
