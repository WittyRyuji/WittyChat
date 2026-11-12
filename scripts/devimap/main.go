// devimap: a dev-only IMAP server on 127.0.0.1:1143 (plaintext), for testing
// the admin mail-import UI against realistic mailboxes. Standalone binary —
// never part of the server/worker.
//
//	devimap                       # listens on 127.0.0.1:1143
//
// Users (login / password):
//   - zhang@wittyloong.com / WITTYCHAT_DEV_IMAP_PASSWORD (default: imappass)
//   - old@feishu-old.test  / WITTYCHAT_DEV_OLD_IMAP_PASSWORD (default: oldpass)
//
// 开发用 IMAP：在 127.0.0.1:1143 起内存邮件服务器，供邮件搬家联调。
package main

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

type lit struct {
	r *strings.Reader
	n int64
}

func (l *lit) Read(b []byte) (int, error) { return l.r.Read(b) }
func (l *lit) Size() int64                { return l.n }

func appendMsg(u *imapmemserver.User, mbox, raw string, flags []imap.Flag, when time.Time) {
	if _, err := u.Append(mbox, &lit{strings.NewReader(raw), int64(len(raw))},
		&imap.AppendOptions{Flags: flags, Time: when}); err != nil {
		log.Fatalf("append %s: %v", mbox, err)
	}
}

func mk(u *imapmemserver.User, name string, su []imap.MailboxAttr) {
	if err := u.Create(name, &imap.CreateOptions{SpecialUse: su}); err != nil {
		log.Fatalf("create %s: %v", name, err)
	}
}

// --- fixtures (same builders as the mailimport tests) ----------------

func plain(i int, date time.Time) string {
	return fmt.Sprintf("From: Bob <bob@x.cn>\r\nTo: alice@wittyloong.com\r\n"+
		"Subject: 周会纪要 #%d\r\nDate: %s\r\nMessage-ID: <pl%d@x>\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n第 %d 期周报正文，含中文。\r\n",
		i, date.Format("Mon, 02 Jan 2006 15:04:05 -0700"), i, i)
}

func gbk(i int, date time.Time) string {
	return "From: =?GBK?B?1cXI/Q==?= <zhang@example.cn>\r\nTo: alice@wittyloong.com\r\n" +
		fmt.Sprintf("Subject: =?GBK?B?u+HS6cbv1tC%doaM=?=\r\n", i%10) +
		"Content-Type: text/plain; charset=GBK\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("\xD5\xE2\xCA\xC7\xD5\xFD\xCE\xC4")) + "\r\n"
}

func nested(i int, date time.Time) string {
	return fmt.Sprintf("From: x@y.z\r\nTo: alice@wittyloong.com\r\nSubject: nested %d\r\n"+
		"Date: %s\r\nMessage-ID: <n%d@x>\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/mixed; boundary=M1\r\n\r\n"+
		"--M1\r\nContent-Type: multipart/related; boundary=M2\r\n\r\n"+
		"--M2\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<img src=\"cid:logo@x\"> <b>hi %d</b>\r\n"+
		"--M2\r\nContent-Type: image/png; name=\"l.png\"\r\nContent-ID: <logo@x>\r\n"+
		"Content-Transfer-Encoding: base64\r\nContent-Disposition: inline; filename=\"l.png\"\r\n\r\n"+
		base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})+"\r\n"+
		"--M2--\r\n"+
		"--M1\r\nContent-Type: application/pdf; name*=UTF-8''%%E6%%8A%%A5%%E5%%91%%8A-%d.pdf\r\n"+
		"Content-Transfer-Encoding: base64\r\n"+
		"Content-Disposition: attachment; filename*=UTF-8''%%E6%%8A%%A5%%E5%%91%%8A-%d.pdf\r\n\r\n"+
		base64.StdEncoding.EncodeToString([]byte("%PDF fake"))+"\r\n--M1--\r\n",
		i, date.Format("Mon, 02 Jan 2006 15:04:05 -0700"), i, i, i, i)
}

func htmlOnly(i int, date time.Time) string {
	return fmt.Sprintf("From: h@x\r\nTo: alice@wittyloong.com\r\nSubject: html %d\r\n"+
		"Date: %s\r\nMessage-ID: <h%d@x>\r\nContent-Type: text/html; charset=utf-8\r\n\r\n"+
		"<p><b>HTML-only %d</b> 内容</p>\r\n",
		i, date.Format("Mon, 02 Jan 2006 15:04:05 -0700"), i, i)
}

func sentMsg(i int, date time.Time) string {
	return fmt.Sprintf("From: zhang@wittyloong.com\r\nTo: out@x\r\nSubject: 已发送 %d\r\n"+
		"Date: %s\r\nMessage-ID: <s%d@x>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n发送的邮件 %d\r\n",
		i, date.Format("Mon, 02 Jan 2006 15:04:05 -0700"), i, i)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	addr := "127.0.0.1:1143"
	zhang := imapmemserver.NewUser("zhang@wittyloong.com", envOr("WITTYCHAT_DEV_IMAP_PASSWORD", "imappass"))
	mk(zhang, "INBOX", nil)
	mk(zhang, "已发送", []imap.MailboxAttr{"\\Sent"})
	mk(zhang, "项目/2024", nil)
	mk(zhang, "Trash", nil)
	base := time.Now().Add(-90 * 24 * time.Hour)
	for i := 0; i < 300; i++ {
		d := base.Add(time.Duration(i) * 8 * time.Hour)
		var raw string
		switch i % 10 {
		case 3:
			raw = gbk(i, d)
		case 6:
			raw = nested(i, d)
		case 9:
			raw = htmlOnly(i, d)
		default:
			raw = plain(i, d)
		}
		var flags []imap.Flag
		if i%7 == 0 {
			flags = []imap.Flag{"\\Seen", "\\Flagged"}
		} else if i%5 == 0 {
			flags = []imap.Flag{"\\Seen"}
		}
		appendMsg(zhang, "INBOX", raw, flags, d)
	}
	for i := 0; i < 10; i++ {
		appendMsg(zhang, "已发送", sentMsg(i, base.Add(time.Duration(i)*time.Hour)), nil, base.Add(time.Duration(i)*time.Hour))
	}
	for i := 0; i < 5; i++ {
		appendMsg(zhang, "项目/2024", plain(100+i, base), nil, base)
	}
	for i := 0; i < 3; i++ {
		appendMsg(zhang, "Trash", plain(200+i, base), nil, base)
	}

	old := imapmemserver.NewUser("old@feishu-old.test", envOr("WITTYCHAT_DEV_OLD_IMAP_PASSWORD", "oldpass"))
	mk(old, "INBOX", nil)
	for i := 0; i < 20; i++ {
		appendMsg(old, "INBOX", plain(300+i, base), nil, base)
	}

	mem := imapmemserver.New()
	mem.AddUser(zhang)
	mem.AddUser(old)
	srv := imapserver.New(&imapserver.Options{
		InsecureAuth: true,
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
	})
	_ = tls.Config{} // keep the import honest; plain only in dev
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("devimap on %s — zhang@wittyloong.com / old@feishu-old.test", addr)
	log.Fatal(srv.Serve(ln))
}
