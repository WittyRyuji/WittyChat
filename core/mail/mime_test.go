package mail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// collectAttachments runs ParseMessage and records each attachment's meta +
// body bytes (spooled like the worker does). / 收集各附件元信息与字节。
func collectAttachments(t *testing.T, raw string) (*Parsed, []AttachmentMeta, [][]byte) {
	t.Helper()
	var metas []AttachmentMeta
	var bodies [][]byte
	p, err := ParseMessage(strings.NewReader(raw), func(m AttachmentMeta, r io.Reader) error {
		metas = append(metas, m)
		b, err := io.ReadAll(r)
		bodies = append(bodies, b)
		return err
	})
	if err != nil {
		t.Fatalf("ParseMessage: %v", err)
	}
	return p, metas, bodies
}

func TestParsePlainUTF8(t *testing.T) {
	raw := "From: Bob <bob@example.com>\r\nTo: alice@wittyloong.com\r\nSubject: Hello\r\n" +
		"Date: Mon, 10 Nov 2026 08:00:00 +0800\r\nMessage-ID: <m1@example.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n你好 Alice，\r\n这是一封测试信。\r\n"
	p, metas, _ := collectAttachments(t, raw)
	if p.Subject != "Hello" || p.From != "bob@example.com" || p.FromName != "Bob" {
		t.Fatalf("hdr: %+v", p)
	}
	if !strings.Contains(p.Text, "你好 Alice") || p.Date.Year() != 2026 || p.MessageID != "m1@example.com" {
		t.Fatalf("body/date/mid: %+v", p)
	}
	if len(p.To) != 1 || p.To[0] != "alice@wittyloong.com" || len(metas) != 0 {
		t.Fatalf("to/atts: %+v %d", p.To, len(metas))
	}
}

func TestParseGBKEncoded(t *testing.T) {
	// GBK-encoded subject + body via encoded words + base64.
	subj := "=?GBK?B?u+HS6Q==?="                                  // 会议
	from := "=?GBK?B?1cXI/Q==?= <li@example.com>"                 // 张三
	bodyGBK, _ := base64.StdEncoding.DecodeString("1KSwobzFstI=") // GBK bytes for 你好
	_ = bodyGBK
	raw := fmt.Sprintf("From: %s\r\nTo: alice@wittyloong.com\r\nSubject: %s\r\n"+
		"Content-Type: text/plain; charset=GBK\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
		from, subj, base64.StdEncoding.EncodeToString([]byte("\xC4\xE3\xBA\xC3"))) // GBK 你好
	p, _, _ := collectAttachments(t, raw)
	if p.Subject != "会议" {
		t.Fatalf("subject %q want 会议", p.Subject)
	}
	if p.FromName != "张三" || p.From != "li@example.com" {
		t.Fatalf("from %q %q", p.FromName, p.From)
	}
	if !strings.Contains(p.Text, "你好") {
		t.Fatalf("gbk body %q", p.Text)
	}
}

func TestParseMultipartAlternative(t *testing.T) {
	raw := "From: x@y.z\r\nSubject: alt\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=B1\r\n\r\n" +
		"--B1\r\nContent-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\nhello=20world\r\n" +
		"--B1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>hello <b>world</b></p>\r\n--B1--\r\n"
	p, metas, _ := collectAttachments(t, raw)
	if !strings.Contains(p.Text, "hello world") {
		t.Fatalf("qp text %q", p.Text)
	}
	if !strings.Contains(p.HTML, "<b>world</b>") || len(metas) != 0 {
		t.Fatalf("html/atts: %q %d", p.HTML, len(metas))
	}
}

func TestParseNestedInlineCID(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	pdf := []byte("%PDF-1.4 fake")
	raw := "From: x@y.z\r\nSubject: nested\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=M1\r\n\r\n" +
		"--M1\r\nContent-Type: multipart/related; boundary=M2\r\n\r\n" +
		"--M2\r\nContent-Type: multipart/alternative; boundary=M3\r\n\r\n" +
		"--M3\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nsee attached\r\n" +
		"--M3\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<img src=\"cid:logo@x\"> hi\r\n" +
		"--M3--\r\n" +
		"--M2\r\nContent-Type: image/png; name=\"logo.png\"\r\n" +
		"Content-ID: <logo@x>\r\nContent-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: inline; filename=\"logo.png\"\r\n\r\n" +
		base64.StdEncoding.EncodeToString(png) + "\r\n" +
		"--M2--\r\n" +
		"--M1\r\nContent-Type: application/pdf; name*=UTF-8''%E6%8A%A5%E5%91%8A.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename*=UTF-8''%E6%8A%A5%E5%91%8A.pdf\r\n\r\n" +
		base64.StdEncoding.EncodeToString(pdf) + "\r\n" +
		"--M1--\r\n"
	p, metas, bodies := collectAttachments(t, raw)
	if len(metas) != 2 {
		t.Fatalf("atts=%d want 2 (%+v)", len(metas), metas)
	}
	var logo, rep *AttachmentMeta
	for i := range metas {
		if metas[i].Mime == "image/png" {
			logo = &metas[i]
			if sha256.Sum256(bodies[i]) != sha256.Sum256(png) {
				t.Fatal("png bytes differ")
			}
		} else {
			rep = &metas[i]
		}
	}
	if logo == nil || !logo.Inline || logo.ContentID != "logo@x" {
		t.Fatalf("logo att: %+v", logo)
	}
	if rep == nil || rep.Filename != "报告.pdf" || rep.Inline || rep.Mime != "application/pdf" {
		t.Fatalf("pdf att: %+v", rep)
	}
	if !strings.Contains(p.HTML, "cid:logo@x") {
		t.Fatalf("html: %q", p.HTML)
	}
}

func TestParseHTMLOnly(t *testing.T) {
	raw := "From: x@y.z\r\nSubject: html only\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<style>body{color:red}</style><p>hi <a href=\"https://x.y\">link</a></p><script>alert(1)</script>"
	p, _, _ := collectAttachments(t, raw)
	if !strings.Contains(p.HTML, "<p>hi") {
		t.Fatalf("html: %q", p.HTML)
	}
	if !strings.Contains(p.Text, "hi link") || strings.Contains(p.Text, "alert") || strings.Contains(p.Text, "color:red") {
		t.Fatalf("derived text: %q", p.Text)
	}
}

func TestParseRFC822Attachment(t *testing.T) {
	inner := "Subject: inner\r\n\r\ninner body"
	raw := "From: x@y.z\r\nSubject: fwd\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B9\r\n\r\n" +
		"--B9\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nsee fwd\r\n" +
		"--B9\r\nContent-Type: message/rfc822\r\n" +
		"Content-Disposition: attachment; filename=\"orig.eml\"\r\n\r\n" +
		strings.ReplaceAll(inner, "\n", "\r\n") + "\r\n--B9--\r\n"
	_, metas, bodies := collectAttachments(t, raw)
	if len(metas) != 1 || metas[0].Mime != "message/rfc822" || metas[0].Filename != "orig.eml" {
		t.Fatalf("eml att: %+v", metas)
	}
	if !strings.Contains(string(bodies[0]), "inner body") {
		t.Fatalf("eml body: %q", bodies[0])
	}
}

func TestParseUnknownCharset(t *testing.T) {
	raw := "From: x@y.z\r\nSubject: x\r\nContent-Type: text/plain; charset=x-unknown-999\r\n\r\nwhatever"
	p, err := ParseMessage(strings.NewReader(raw), nil)
	if err != nil && p == nil {
		t.Fatalf("hard error on unknown charset: %v", err)
	}
	if p == nil || !strings.Contains(p.Text, "whatever") {
		t.Fatalf("partial text: %+v", p)
	}
}

func TestParseMalformedBoundary(t *testing.T) {
	raw := "From: x@y.z\r\nSubject: broken\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=ZZ\r\n\r\n" +
		"--ZZ\r\nContent-Type: text/plain; charset=utf-8\r\n\r\npartial content\r\n" +
		"--ZZ\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nsecond part never closes"
	p, metas, _ := collectAttachments(t, raw)
	if p == nil || !strings.Contains(p.Text, "partial content") {
		t.Fatalf("malformed: %q", p.Text)
	}
	if len(metas) != 0 {
		t.Fatalf("atts: %d", len(metas))
	}
}

// TestDecodeWords: single word, split-multibyte adjacent words, mixed
// text prefix, Q-underscore — the regression cases for decodeWords.
func TestDecodeWords(t *testing.T) {
	if got := decodeWords("=?GBK?B?u+HS6Q==?="); got != "会议" {
		t.Fatalf("single: %q", got)
	}
	// 周五会议改期通知 split across two GBK words mid-character
	if got := decodeWords("=?GBK?B?1tzO5bs=?= =?GBK?B?4dLpuMTG2s2o1qo=?="); got != "周五会议改期通知" {
		t.Fatalf("split: %q", got)
	}
	if got := decodeWords("Re: =?utf-8?Q?hello_主题?="); got != "Re: hello 主题" {
		t.Fatalf("mixed/Q: %q", got)
	}
	if got := decodeWords("plain"); got != "plain" {
		t.Fatalf("plain: %q", got)
	}
	// Folded header: the split lands on a CRLF+space fold.
	raw := "From: x@y.z\r\nSubject: =?GBK?B?1tzO5bs=?=\r\n =?GBK?B?4dLpuMTG2s2o1qo=?=\r\n\r\nbody\r\n"
	p, err := ParseMessage(strings.NewReader(raw), nil)
	if err != nil || p.Subject != "周五会议改期通知" {
		t.Fatalf("folded split: %q err=%v", p.Subject, err)
	}
}

// TestDeliverParsedFields: html_body, content_id/inline, sent_at, flags, and returns a mail Store + a user
// factory. Skips without WITTYCHAT_TEST_DB_URL.
func testMailEnv(t *testing.T) (*Store, context.Context, func() uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("WITTYCHAT_TEST_DB_URL")
	if dsn == "" {
		t.Skip("WITTYCHAT_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	st := NewStore(pool, "wittyloong.com")
	mk := func() uuid.UUID {
		t.Helper()
		var id uuid.UUID
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		em := fmt.Sprintf("mail-%s@example.test", hex.EncodeToString(b))
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (id, email, display_name, status, is_admin)
			 VALUES ($1,$2,$3,'active',false) RETURNING id`,
			uuid.Must(uuid.NewV7()), em, em).Scan(&id); err != nil {
			t.Fatalf("mkUser: %v", err)
		}
		if err := st.EnsureFolders(ctx, id); err != nil {
			t.Fatalf("folders: %v", err)
		}
		return id
	}
	return st, ctx, mk
}

// TestDeliverParsedFields: html_body, content_id/inline, sent_at, flags,
// custom folder — against the real test DB.
func TestDeliverParsedFields(t *testing.T) {
	s, ctx, mk := testMailEnv(t)
	owner := mk()
	fid, err := s.EnsureCustomFolder(ctx, owner, "Import-Feishu")
	if err != nil {
		t.Fatalf("custom folder: %v", err)
	}
	// re-fetch → same id (idempotent)
	fid2, err := s.EnsureCustomFolder(ctx, owner, "Import-Feishu")
	if err != nil || fid2 != fid {
		t.Fatalf("custom folder idempotent: %v %v", fid2, err)
	}
	p := &Parsed{
		From: "bob@example.com", To: []string{"alice@wittyloong.com"},
		Subject: "migrated", Text: "hello", HTML: "<p>hello</p>",
		Date: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	var att uuid.UUID
	attSha := sha256.Sum256([]byte("ab12"))
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO objects (id,owner_user,name,size_bytes,sha256,mime,location,status,dims_checked)
		 VALUES ($1,$2,'logo.png',4,$3,'image/png','local','ready',true) RETURNING id`,
		uuid.Must(uuid.NewV7()), owner, attSha[:]).Scan(&att); err != nil {
		t.Fatal(err)
	}
	id, err := s.DeliverParsed(ctx, owner, "", &fid, p, DeliverOpts{
		IsRead: false, IsStarred: true,
		SentAt:      p.Date,
		SizeBytes:   4096,
		Direction:   "in",
		Attachments: []DeliveredAttachment{{ObjectID: att, ContentID: "logo@x", Inline: true}},
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	var html, dir string
	var sentAt time.Time
	var starred, read bool
	var size int64
	if err := s.pool.QueryRow(ctx,
		`SELECT html_body,direction,sent_at,is_read,is_starred,size_bytes FROM mail_messages WHERE id=$1`, id).
		Scan(&html, &dir, &sentAt, &read, &starred, &size); err != nil {
		t.Fatal(err)
	}
	if html != "<p>hello</p>" || dir != "in" || !sentAt.Equal(p.Date) || read || !starred || size != 4096 {
		t.Fatalf("row: html=%q dir=%s sent=%v read=%v star=%v size=%d", html, dir, sentAt, read, starred, size)
	}
	var cid *string
	var inl bool
	if err := s.pool.QueryRow(ctx,
		`SELECT content_id,inline FROM mail_attachments WHERE message_id=$1`, id).Scan(&cid, &inl); err != nil {
		t.Fatal(err)
	}
	if cid == nil || *cid != "logo@x" || !inl {
		t.Fatalf("att: cid=%v inl=%v", cid, inl)
	}
}
