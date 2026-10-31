package mail

// Outbound path: direct-to-MX SMTP delivery (net/smtp + net.LookupMX,
// STARTTLS when offered). Inbound parsing for the worker's mail_parse
// task — maddy drops raw .eml into objects, worker parses into mailbox.
//
// 出站：直连对方 MX。入站：maddy 落盘 .eml → worker 解析入库。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/google/uuid"
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

// ParsedMail is a minimal RFC822 parse result (headers + text body).
type ParsedMail struct {
	From      string
	To        []string
	Cc        []string
	Subject   string
	MessageID string
	InReplyTo string
	Body      string
}

// ParseRaw is a deliberately small parser: unfolds headers, splits body,
// strips a single MIME multipart boundary level for text/plain parts.
func ParseRaw(raw []byte) (*ParsedMail, error) {
	r := bufio.NewReader(strings.NewReader(string(raw)))
	hdr := map[string]string{}
	var last string
	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return nil, io.ErrUnexpectedEOF
		}
		trim := strings.TrimRight(line, "\r\n")
		if trim == "" {
			break
		}
		if strings.HasPrefix(trim, " ") || strings.HasPrefix(trim, "\t") {
			hdr[last] += " " + strings.TrimSpace(trim)
			continue
		}
		i := strings.Index(trim, ":")
		if i < 0 {
			continue
		}
		last = strings.ToLower(trim[:i])
		hdr[last] = strings.TrimSpace(trim[i+1:])
	}
	rest, _ := io.ReadAll(r)
	m := &ParsedMail{
		From:      hdr["from"],
		Subject:   hdr["subject"],
		MessageID: hdr["message-id"],
		InReplyTo: hdr["in-reply-to"],
		To:        splitAddrs(hdr["to"]),
		Cc:        splitAddrs(hdr["cc"]),
	}
	ct := hdr["content-type"]
	if strings.HasPrefix(ct, "multipart/") {
		if i := strings.Index(ct, "boundary="); i >= 0 {
			b := strings.Trim(ct[i+9:], `"`)
			m.Body = firstTextPart(string(rest), b)
		} else {
			m.Body = string(rest)
		}
	} else {
		m.Body = strings.TrimSpace(string(rest))
	}
	return m, nil
}

func splitAddrs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if i := strings.LastIndex(p, "<"); i >= 0 && strings.HasSuffix(p, ">") {
			p = p[i+1 : len(p)-1]
		}
		if p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return out
}

func firstTextPart(body, boundary string) string {
	for _, part := range strings.Split(body, "--"+boundary) {
		if strings.Contains(part, "Content-Type: text/plain") {
			if i := strings.Index(part, "\r\n\r\n"); i >= 0 {
				return strings.TrimSpace(part[i+4:])
			}
		}
	}
	return strings.TrimSpace(body)
}

// Inbound drops a parsed external message into matching local mailboxes.
func (s *Store) Inbound(ctx context.Context, m *ParsedMail, rawObjID uuid.UUID) (int, error) {
	delivered := 0
	all := append(append([]string{}, m.To...), m.Cc...)
	for _, addr := range all {
		if !s.IsLocal(addr) {
			continue
		}
		uid, err := s.UserForAddress(ctx, addr)
		if err != nil {
			continue
		}
		folder := "inbox"
		if id, err := s.Deliver(ctx, uid, folder, m.From, m.To, m.Cc,
			m.Subject, m.Body, m.MessageID, m.InReplyTo, nil); err == nil {
			_, _ = s.pool.Exec(ctx,
				`UPDATE mail_messages SET raw_object_id=$1 WHERE id=$2`, rawObjID, id)
			delivered++
		}
	}
	return delivered, nil
}
