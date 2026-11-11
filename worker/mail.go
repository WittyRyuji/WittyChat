package main

// Worker mail tasks: mail_send (direct-to-MX external delivery) and
// mail_parse (inbound .eml from objects → local mailboxes).
//
// mail_send：外发直连对方 MX。mail_parse：入站 .eml 解析入库。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"
	"wittychat/core/mail"
	"wittychat/core/settings"

	"github.com/google/uuid"
)

func (w *worker) mailStore() *mail.Store {
	if w.mail == nil {
		w.mail = mail.NewStore(w.pool, envOr("WITTYCHAT_MAIL_DOMAIN", "wittyloong.com"))
	}
	return w.mail
}

// mailSend re-renders the sent message as RFC822 and MX-delivers to one rcpt.
func (w *worker) mailSend(ctx context.Context, msgID uuid.UUID, rcpt string) error {
	var from, subject, body, rfcID, inReplyTo string
	var to, cc []string
	err := w.pool.QueryRow(ctx,
		`SELECT from_addr,to_addrs,cc_addrs,subject,text_body,
		        COALESCE(rfc_msgid,''),COALESCE(in_reply_to,'')
		 FROM mail_messages WHERE id=$1 AND direction='out'`, msgID).
		Scan(&from, &to, &cc, &subject, &body, &rfcID, &inReplyTo)
	if err != nil {
		return err
	}
	raw := mail.BuildRFC822(from, to, cc, subject, body, rfcID, inReplyTo)
	// DKIM-sign when a key is configured — unsigned mail lands in spam at
	// real providers. A configured-but-broken key fails loudly so the task
	// retry surfaces the config error instead of silently sending unsigned.
	// 配置了密钥就签 DKIM：真邮局对未签名件基本判垃圾。密钥配置错误时
	// 让任务报错重试，暴露配置问题，而不是静默发未签名件。
	st := settings.New(w.pool)
	if keyPEM := st.Str(ctx, settings.KeyMailDKIMKey, ""); keyPEM != "" {
		sel := st.Str(ctx, settings.KeyMailDKIMSelector, "s1")
		signed, err := mail.SignDKIM(raw, w.mailStore().LocalDomain, sel, []byte(keyPEM))
		if err != nil {
			return fmt.Errorf("dkim sign: %w", err)
		}
		raw = signed
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := mail.SendMX(ctx, from, []string{rcpt}, raw); err != nil {
		return fmt.Errorf("mx send to %s: %w", rcpt, err)
	}
	return nil
}

// mailParse reads the raw .eml object, parses it fully (MIME tree,
// charsets, attachments), stores attachments as objects and delivers one
// mailbox copy per local recipient.
// 入站解析：完整走 MIME 树（编码、附件），附件入对象库，逐本地收件人入库。
func (w *worker) mailParse(ctx context.Context, objID uuid.UUID) error {
	m := w.mailStore()
	obj, err := w.store.Peek(ctx, objID)
	if err != nil {
		return err
	}
	rc, err := w.store.Open(ctx, obj)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	// Spool attachment bytes to temp files during the walk — reusing them
	// per recipient avoids holding large blobs in memory.
	// 附件先落临时文件：按收件人复用，避免大附件占内存。
	tmpDir, err := os.MkdirTemp("", "mailparse-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	var atts []struct {
		meta mail.AttachmentMeta
		path string
		size int64
	}
	pm, err := mail.ParseMessage(rc,
		func(meta mail.AttachmentMeta, r io.Reader) error {
			f, err := os.CreateTemp(tmpDir, "att-*")
			if err != nil {
				log.Printf("mail: spool attachment %q: %v", meta.Filename, err)
				return err
			}
			n, err := io.Copy(f, r)
			_ = f.Close()
			if err != nil {
				log.Printf("mail: spool attachment %q: %v", meta.Filename, err)
				return err
			}
			atts = append(atts, struct {
				meta mail.AttachmentMeta
				path string
				size int64
			}{meta, f.Name(), n})
			return nil
		})
	if err != nil {
		return err
	}

	// Resolve local recipients first — attachments are put under each.
	// 先解析本地收件人：附件按属主逐个入库（Put 按 sha 去重）。
	recipients := []uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	for _, addr := range append(append([]string{}, pm.To...), pm.Cc...) {
		if !m.IsLocal(addr) {
			continue
		}
		if uid, err := m.UserForAddress(ctx, addr); err == nil && !seen[uid] {
			seen[uid] = true
			recipients = append(recipients, uid)
		}
	}
	if len(recipients) == 0 {
		return errors.New("no local recipients matched / 无本地收件人")
	}

	delivered := 0
	for _, uid := range recipients {
		var dAtts []mail.DeliveredAttachment
		for _, a := range atts {
			f, err := os.Open(a.path)
			if err != nil {
				continue
			}
			o, _, err := w.store.Put(ctx, uid, uuid.Nil, "mail", a.meta.Filename,
				a.meta.Mime, f, a.size+1)
			_ = f.Close()
			if err != nil {
				log.Printf("mail: put attachment %q for %s: %v", a.meta.Filename, uid, err)
				continue
			}
			dAtts = append(dAtts, mail.DeliveredAttachment{
				ObjectID: o.ID, ContentID: a.meta.ContentID, Inline: a.meta.Inline,
			})
		}
		if _, err := m.DeliverParsed(ctx, uid, "inbox", nil, pm, mail.DeliverOpts{
			RawObjectID: &objID, Attachments: dAtts,
			SentAt: pm.Date, SizeBytes: obj.Size,
		}); err == nil {
			delivered++
		}
	}
	if delivered == 0 {
		return errors.New("deliver failed for all local recipients / 全部投递失败")
	}
	return nil
}
