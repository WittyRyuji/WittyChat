// MIME parsing for inbound/migrated mail: full RFC 5322+2045+2047 walk via
// go-message, with GBK/Big5 charsets registered through go-message/charset.
// Best-effort — a malformed message yields whatever parsed, never a panic.
//
// 邮件 MIME 解析：完整走 RFC 5322+2045+2047 树，charset 包注册 GBK/Big5
// 等编码。尽力而为——畸形邮件返回已解析部分，绝不 panic。

package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset" // registers GBK/Big5/… via x/text / 注册 GBK/Big5 等编码
	"github.com/emersion/go-message/mail"
)

// decodeWords decodes RFC2047 encoded words in s. Adjacent encoded words
// separated only by whitespace AND sharing a charset have their decoded
// BYTES concatenated before a single charset→UTF-8 conversion — that is
// the only way to survive encoders that split a multibyte character
// across two words (the case both stdlib and go-message miss).
// decodeWords 解码 RFC2047 编码词：相邻（仅空白分隔）且同编码的词，先拼
// 解码后的字节再统一转 UTF-8——能扛住把多字节字符切成两半的编码器
// （stdlib 和 go-message 都会丢成 U+FFFD）。
var reWord = regexp.MustCompile(`=\?([^?\s]+)\?([bBqQ])\?([^?]*)\?=`)

type wordTok struct {
	charset, enc string
	raw          []byte
	start, end   int
}

func decodeWords(s string) string {
	ms := reWord.FindAllStringSubmatchIndex(s, -1)
	if ms == nil {
		return s
	}
	var toks []wordTok
	for _, m := range ms {
		charset := strings.ToLower(s[m[2]:m[3]])
		enc := s[m[4]:m[5]]
		text := s[m[6]:m[7]]
		var raw []byte
		if enc == "b" || enc == "B" {
			b, err := base64.StdEncoding.DecodeString(text)
			if err != nil {
				raw = nil
			} else {
				raw = b
			}
		} else {
			raw = qDecode(text)
		}
		if raw == nil {
			continue // leave the word alone / 解不了就原样保留
		}
		toks = append(toks, wordTok{charset, enc, raw, m[0], m[1]})
	}
	if len(toks) == 0 {
		return s
	}
	// Group adjacent toks: whitespace-only gap + same charset.
	groups := [][]wordTok{{toks[0]}}
	for _, t := range toks[1:] {
		last := groups[len(groups)-1]
		prev := last[len(last)-1]
		gap := s[prev.end:t.start]
		// Folding whitespace (CRLF + space) counts as a gap too — long
		// subjects are folded exactly where encoders split words.
		// 折行空白（CRLF+空格）同样算间隔——长主题恰在折行处被拆词。
		if strings.TrimSpace(gap) == "" && t.charset == prev.charset {
			groups[len(groups)-1] = append(last, t)
		} else {
			groups = append(groups, []wordTok{t})
		}
	}
	var out strings.Builder
	pos := 0
	for _, g := range groups {
		out.WriteString(s[pos:g[0].start])
		var cat []byte
		for _, t := range g {
			cat = append(cat, t.raw...)
		}
		r, err := charset.Reader(g[0].charset, bytes.NewReader(cat))
		if err != nil {
			out.WriteString(s[g[0].start:g[len(g)-1].end]) // unknown charset → raw
		} else if b, err := io.ReadAll(r); err != nil {
			out.WriteString(s[g[0].start:g[len(g)-1].end])
		} else {
			out.Write(b)
		}
		pos = g[len(g)-1].end
	}
	out.WriteString(s[pos:])
	return out.String()
}

func qDecode(s string) []byte {
	s = strings.ReplaceAll(s, "_", " ")
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '=' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return out
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Parsed is the decoded top-level view of one message.
type Parsed struct {
	From, FromName string // address lowercased; display name RFC2047-decoded
	To, Cc, Bcc    []string
	Subject        string // RFC2047-decoded
	Date           time.Time
	MessageID      string // without <>
	InReplyTo      string
	References     []string
	Text, HTML     string
}

// AttachmentMeta describes one non-body part.
type AttachmentMeta struct {
	Filename, Mime, ContentID string
	Inline                    bool
}

// ParseMessage reads one message; onAttachment is called for every part
// that isn't a displayable text body (or is marked attachment). A nil
// onAttachment still consumes the part bodies.
func ParseMessage(r io.Reader, onAttachment func(AttachmentMeta, io.Reader) error) (*Parsed, error) {
	e, err := message.Read(r)
	if err != nil && e == nil {
		return nil, err
	}
	p := &Parsed{}
	h := mail.Header{Header: e.Header}
	p.Subject = decodeWords(e.Header.Get("Subject"))
	if addr, aerr := h.AddressList("From"); aerr == nil && len(addr) > 0 {
		p.From = strings.ToLower(addr[0].Address)
		p.FromName = addr[0].Name
	}
	p.To = lowerAddrs(h, "To")
	p.Cc = lowerAddrs(h, "Cc")
	p.Bcc = lowerAddrs(h, "Bcc")
	if d, derr := h.Date(); derr == nil {
		p.Date = d
	}
	if mid, merr := h.MessageID(); merr == nil {
		p.MessageID = mid
	}
	if rt, rerr := h.MsgIDList("In-Reply-To"); rerr == nil && len(rt) > 0 {
		p.InReplyTo = rt[0]
	}
	if refs, rerr := h.MsgIDList("References"); rerr == nil {
		p.References = refs
	}

	var texts []string
	walkPart(e, &texts, p, onAttachment)
	p.Text = strings.TrimSpace(strings.Join(texts, "\n\n"))
	if p.Text == "" && p.HTML != "" {
		p.Text = htmlToText(p.HTML)
	}
	return p, nil
}

func lowerAddrs(h mail.Header, field string) []string {
	list, err := h.AddressList(field)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, strings.ToLower(a.Address))
	}
	return out
}

var attCounter int

func walkPart(e *message.Entity, texts *[]string, p *Parsed, onAtt func(AttachmentMeta, io.Reader) error) {
	if mr := e.MultipartReader(); mr != nil {
		for {
			sub, err := mr.NextPart()
			if err != nil {
				break // EOF or malformed — keep what we have / 尽力而为
			}
			walkPart(sub, texts, p, onAtt)
		}
		return
	}
	ct, _, _ := e.Header.ContentType()
	ct = strings.ToLower(ct)
	disp, dparams, _ := e.Header.ContentDisposition()
	isAttachment := strings.EqualFold(disp, "attachment")
	cid := strings.Trim(strings.TrimSpace(e.Header.Get("Content-ID")), "<>")

	if ct == "text/plain" && !isAttachment && cid == "" {
		if b := strings.TrimSpace(readBody(e)); b != "" {
			*texts = append(*texts, b)
		}
		return
	}
	if ct == "text/html" && !isAttachment && cid == "" {
		if p.HTML == "" {
			p.HTML = readBody(e)
		} else {
			_, _ = io.Copy(io.Discard, e.Body)
		}
		return
	}

	// Everything else is an attachment — including message/rfc822 (.eml).
	// 其余一律按附件处理（message/rfc822 即 .eml 附件）。
	ah := mail.AttachmentHeader{Header: e.Header}
	name, _ := ah.Filename() // RFC2231/2047 decoded; falls back to name=
	if name == "" {
		name = dparams["filename"]
	}
	if strings.Contains(name, "=?") {
		name = decodeWords(name)
	}
	if name == "" {
		ext := "bin"
		if i := strings.LastIndex(ct, "/"); i >= 0 {
			ext = ct[i+1:]
		}
		if ct == "message/rfc822" {
			ext = "eml"
		}
		attCounter++
		name = fmt.Sprintf("part-%d.%s", attCounter, ext)
	}
	meta := AttachmentMeta{
		Filename: name, Mime: ct,
		ContentID: cid,
		Inline:    cid != "" && !isAttachment,
	}
	if onAtt != nil {
		_ = onAtt(meta, e.Body)
	} else {
		_, _ = io.Copy(io.Discard, e.Body)
	}
}

// readBody drains a text part body — go-message already decoded transfer
// encoding + charset at Read time. Capped at 8 MiB; the remainder is
// drained and a truncation marker appended.
// 正文 8MiB 封顶，超出丢弃并追加截断标记。
const maxBodyPart = 8 << 20

func readBody(e *message.Entity) string {
	lr := &io.LimitedReader{R: e.Body, N: maxBodyPart}
	b, _ := io.ReadAll(lr)
	if lr.N == 0 {
		_, _ = io.Copy(io.Discard, e.Body) // finish the part / 读完剩余
		return string(b) + "\n[…正文过长已截断 / body truncated]"
	}
	return string(b)
}

var (
	reScript = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	reBr     = regexp.MustCompile(`(?i)<br\s*/?>`)
	reBlock  = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6])\s*>`)
	reTags   = regexp.MustCompile(`<[^>]+>`)
	reWS     = regexp.MustCompile(`[ \t]+`)
	reBlank  = regexp.MustCompile(`\n{3,}`)
)

// htmlToText derives a readable plain-text body from HTML — stdlib only.
// script/style contents are dropped first, block closers become newlines,
// tags are stripped and entities unescaped.
// HTML→纯文本：先丢 script/style 内容，块级闭合转换行，去标签、解实体，
// 合并多余空白。
func htmlToText(h string) string {
	t := reScript.ReplaceAllString(h, "")
	t = reBr.ReplaceAllString(t, "\n")
	t = reBlock.ReplaceAllString(t, "\n")
	t = reTags.ReplaceAllString(t, "")
	t = html.UnescapeString(t)
	lines := strings.Split(t, "\n")
	for i, l := range lines {
		lines[i] = reWS.ReplaceAllString(strings.TrimSpace(l), " ")
	}
	return strings.TrimSpace(reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
