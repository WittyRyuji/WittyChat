package mail

// Outbound path: direct-to-MX SMTP delivery (net/smtp + net.LookupMX,
// STARTTLS when offered). Inbound parsing for the worker's mail_parse
// task — maddy drops raw .eml into objects, worker parses into mailbox.
//
// 出站：直连对方 MX。入站：maddy 落盘 .eml → worker 解析入库。

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SendMX delivers one RFC822 message to each external recipient's MX.
// Grouped by domain; first MX that answers wins.
func SendMX(ctx context.Context, from string, rcpts []string, raw []byte) error {
	byDomain := map[string][]string{}
	for _, r := range rcpts {
		i := strings.LastIndex(r, "@")
		if i < 0 {
			continue
		}
		d := strings.ToLower(r[i+1:])
		byDomain[d] = append(byDomain[d], r)
	}
	var firstErr error
	for domain, list := range byDomain {
		if err := sendToDomain(ctx, domain, from, list, raw); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func sendToDomain(ctx context.Context, domain, from string, rcpts []string, raw []byte) error {
	mxs, err := net.LookupMX(domain)
	if err != nil || len(mxs) == 0 {
		// No MX → try the domain itself (RFC 5321 implicit MX).
		mxs = []*net.MX{{Host: domain + "."}}
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for _, mx := range mxs {
		if time.Now().After(deadline) {
			break
		}
		host := strings.TrimSuffix(mx.Host, ".")
		err := func() error {
			d := net.Dialer{Timeout: 10 * time.Second}
			conn, err := d.DialContext(ctx, "tcp", host+":25")
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			c, err := smtp.NewClient(conn, host)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			if ok, _ := c.Extension("STARTTLS"); ok {
				if err := c.StartTLS(nil); err != nil { //nolint:staticcheck // dev path
					return err
				}
			}
			if err := c.Mail(from); err != nil {
				return err
			}
			for _, r := range rcpts {
				if err := c.Rcpt(r); err != nil {
					return err
				}
			}
			w, err := c.Data()
			if err != nil {
				return err
			}
			if _, err := w.Write(raw); err != nil {
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
			return c.Quit()
		}()
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("all MX for %s failed: %w", domain, lastErr)
}

// BuildRFC822 assembles a minimal text/plain message.
func BuildRFC822(from string, to, cc []string, subject, body, msgID string, inReplyTo string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\n", from, strings.Join(to, ", "))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	if len(cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\r\nMessage-ID: %s\r\n", subject, msgID)
	if inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\nReferences: %s\r\n", inReplyTo, inReplyTo)
	}
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", body)
	return []byte(b.String())
}
