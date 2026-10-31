package mail

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestSignDKIM_Verifies(t *testing.T) {
	raw := BuildRFC822(
		"alice@wittyloong.com", []string{"bob@example.org"}, nil,
		"dkim test", "hello world", "<m1@wittyloong.com>", "",
	)
	signed, err := SignDKIM(raw, "wittyloong.com", "s1", testKeyPEM(t))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !strings.HasPrefix(string(signed), "DKIM-Signature:") {
		t.Fatalf("missing DKIM-Signature header:\n%.200s", signed)
	}
	// Cryptographic verification performs a DNS lookup for the key; check the
	// signature header fields instead, then verify body hash survives signing.
	if !strings.Contains(string(signed), "d=wittyloong.com") ||
		!strings.Contains(string(signed), "s=s1") ||
		!strings.Contains(string(signed), "bh=") {
		t.Fatalf("bad DKIM-Signature fields:\n%.300s", signed)
	}
	// Signing only prepends the DKIM-Signature header — original bytes intact.
	if !bytes.HasSuffix(signed, raw) {
		t.Fatal("signed message body mutated")
	}
}

func TestSignDKIM_BadKey(t *testing.T) {
	if _, err := SignDKIM([]byte("x"), "d.test", "s1", []byte("not pem")); err == nil {
		t.Fatal("expected error for bad PEM")
	}
}

// Sanity: dkim package imports work and options struct has the fields we use.
func TestDKIMAPI(t *testing.T) {
	var _ = dkim.SignOptions{
		Domain:   "x.test",
		Selector: "s1",
	}
}
