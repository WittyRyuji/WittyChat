package auth

// Argon2id password hashing in PHC string format.
// Argon2id 密码哈希（PHC 字符串格式）。

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword hashes pw with argon2id and returns a PHC string.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a PHC hash in constant time.
func VerifyPassword(pw, phc string) (bool, error) {
	parts := strings.Split(phc, "$")
	// "$argon2id$v=19$m=...,t=...,p=...$salt$key" → ["",argon2id,v=19,params,salt,key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("invalid phc format")
	}
	var mem, rounds uint32
	var par uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &rounds, &par); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	if len(want) == 0 || len(want) > 1024 {
		return false, errors.New("invalid key length")
	}
	got := argon2.IDKey([]byte(pw), salt, rounds, mem, par, uint32(len(want))) //nolint:gosec // bounded above
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
