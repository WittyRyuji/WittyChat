package objects

// Short-lived signed download URLs: sig = HMAC-SHA256(secret, id|exp).
// Lets clients embed images/files in <img>/mail contexts where an
// Authorization header can't ride along.
//
// 短时效签名下载 URL，用于 <img>、邮件正文等无法带鉴权头的场景。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Signer mints/verifies signed object URLs using the server secret key.
type Signer struct {
	key []byte
}

func NewSigner(key []byte) *Signer { return &Signer{key: key} }

// URL returns "exp=<unix>&sig=<b64>" query params valid for ttl.
func (s *Signer) Params(id uuid.UUID, ttl time.Duration) (exp int64, sig string) {
	exp = time.Now().Add(ttl).Unix()
	mac := hmac.New(sha256.New, s.key)
	_, _ = fmt.Fprintf(mac, "%s|%d", id.String(), exp)
	return exp, base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify checks exp/sig params against the object id.
func (s *Signer) Verify(id uuid.UUID, expStr, sig string) bool {
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = fmt.Fprintf(mac, "%s|%d", id.String(), exp)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// SignSubject mints exp/sig params over an arbitrary subject string —
// same HMAC model as Params but the caller composes what's being granted
// (e.g. "office-edit|obj|uid"). 任意主体的 exp/sig 签名，粒度由调用方定。
func (s *Signer) SignSubject(subject string, ttl time.Duration) (exp int64, sig string) {
	exp = time.Now().Add(ttl).Unix()
	mac := hmac.New(sha256.New, s.key)
	_, _ = fmt.Fprintf(mac, "%s|%d", subject, exp)
	return exp, base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifySubject checks exp/sig against the subject string.
func (s *Signer) VerifySubject(subject, expStr, sig string) bool {
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = fmt.Fprintf(mac, "%s|%d", subject, exp)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// HexSHA formats a sha256 sum for logs/debug.
func HexSHA(sha [32]byte) string { return hex.EncodeToString(sha[:]) }

// ParseSHA parses a hex sha256 into the fixed array (upload dedup pre-check).
func ParseSHA(h string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(strings.TrimSpace(h))
	if err != nil || len(raw) != 32 {
		return out, fmt.Errorf("bad sha256 hex")
	}
	copy(out[:], raw)
	return out, nil
}
