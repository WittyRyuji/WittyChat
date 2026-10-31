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

// mailParse reads the raw .eml object, parses, delivers to local mailboxes.
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
	raw, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	if err != nil {
		return err
	}
	pm, err := mail.ParseRaw(raw)
	if err != nil {
		return err
	}
	n, err := m.Inbound(ctx, pm, objID)
	if n == 0 && err == nil {
		return errors.New("no local recipients matched / 无本地收件人")
	}
	return err
}
