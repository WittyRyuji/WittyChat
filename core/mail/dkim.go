package mail

// DKIM signing for outbound mail (RFC 6376, rsa-sha256, relaxed/relaxed).
// Unsigned mail to Gmail/Outlook lands in spam or gets rejected — signing is
// mandatory for a self-hosted sender. Key pair lives in workspace_settings
// (generated via `server dkim`); DNS TXT must be published by the operator.
//
// 出站邮件 DKIM 签名：不签 Gmail/Outlook 基本进垃圾箱或拒收——自托管发件
// 必配。密钥存 workspace_settings（server dkim 生成），DNS TXT 由运维发布。

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMHeaders — the h= list; keep in sync with BuildRFC822's output.
var DKIMHeaders = []string{
	"From", "To", "Cc", "Subject", "Date", "Message-ID",
	"MIME-Version", "Content-Type",
}

// SignDKIM signs a complete RFC822 message, returning it with a
// DKIM-Signature header prepended.
func SignDKIM(raw []byte, domain, selector string, keyPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("dkim: bad PEM block")
	}
	key, err := parseRSAKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = dkim.Sign(&out, bytes.NewReader(raw), &dkim.SignOptions{
		Domain:                 domain,
		Selector:               selector,
		Signer:                 key,
		HeaderKeys:             DKIMHeaders,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
	})
	if err != nil {
		return nil, fmt.Errorf("dkim sign: %w", err)
	}
	return out.Bytes(), nil
}

func parseRSAKey(der []byte) (*rsa.PrivateKey, error) {
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	k8, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("dkim: unparseable private key")
	}
	key, ok := k8.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("dkim: not an RSA key")
	}
	return key, nil
}
