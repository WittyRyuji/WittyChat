package feishuimport

// Per-person mail migration through the Feishu mail API with the app's
// tenant token — no employee passwords. Each message comes from batch_get:
// when Feishu returns the raw MIME it goes through the same parser as
// inbound mail (raw kept); otherwise the message is rebuilt from fields.
// Progress is a page cursor per folder; incremental passes stop a folder at
// the first page that is entirely imported, full passes walk everything.
// 按人搬邮件（应用凭据，无需员工密码）：有原始 MIME 走入站同一解析器并保存原文，
// 否则按字段重建。每个文件夹记分页游标；增量轮次遇到整页已导入即停，全量轮次走完。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"wittychat/core/mail"

	"github.com/google/uuid"
)

var errMailboxDenied = errors.New("应用无权读取该邮箱：请在开发者后台为应用配置邮件数据权限范围（包含该成员）")

type fsAddr struct {
	Address string `json:"mail_address"`
	Name    string `json:"name"`
}

type fsAttachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Body     string `json:"body"`
	Type     int    `json:"attachment_type"`
	Inline   bool   `json:"is_inline"`
	CID      string `json:"cid"`
	Mime     string `json:"content_type"`
}

type fsMessage struct {
	MessageID    string         `json:"message_id"`
	Raw          string         `json:"raw"`
	Subject      string         `json:"subject"`
	To           []fsAddr       `json:"to"`
	Cc           []fsAddr       `json:"cc"`
	Bcc          []fsAddr       `json:"bcc"`
	From         fsAddr         `json:"head_from"`
	HTML         string         `json:"body_html"`
	Text         string         `json:"body_plain_text"`
	InternalDate string         `json:"internal_date"`
	State        int            `json:"message_state"`
	SMTPID       string         `json:"smtp_message_id"`
	Attachments  []fsAttachment `json:"attachments"`
	Labels       []string       `json:"label_ids"`
	FolderID     string         `json:"folder_id"`
	InReplyTo    string         `json:"in_reply_to"`
	References   string         `json:"references"`
}

type mailFolder struct {
	ID, Name, Kind string
	Target         uuid.UUID
}

type mailPerson struct {
	OpenID, Mailbox string
	User            uuid.UUID
	Full            bool
}

// b64url decodes Feishu's base64url fields (padding optional).
func b64url(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	return base64.RawURLEncoding.DecodeString(s)
}

func mailKey(mailbox, id string) string {
	h := sha256.Sum256([]byte("feishu-mail\x00" + strings.ToLower(mailbox) + "\x00" + id))
	return hex.EncodeToString(h[:])
}

// systemKind maps Feishu system folder ids onto WittyChat folder kinds.
func systemKind(id string) (kind, custom string) {
	switch strings.ToUpper(id) {
	case "INBOX":
		return "inbox", ""
	case "SENT":
		return "sent", ""
	case "DRAFT", "DRAFTS":
		return "drafts", ""
	case "SPAM":
		return "spam", ""
	case "ARCHIVED", "ARCHIVE":
		return "archive", ""
	case "TRASH", "DELETED":
		return "trash", ""
	case "STRANGER":
		return "custom", "陌生人"
	}
	return "", ""
}

// folders lists the person's Feishu folders and resolves each to a local
// folder (system → kind folder, user folders → custom folder with its path).
func (s *Store) mailFolders(ctx context.Context, c *fc, p mailPerson) ([]mailFolder, error) {
	var out struct {
		Items []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Parent string `json:"parent_folder_id"`
			Type   int    `json:"folder_type"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/open-apis/mail/v1/user_mailboxes/"+url.PathEscape(p.Mailbox)+"/folders", &out); err != nil {
		return nil, err
	}
	names := map[string]string{}
	parents := map[string]string{}
	for _, f := range out.Items {
		names[f.ID], parents[f.ID] = f.Name, f.Parent
	}
	pathOf := func(id string) string {
		var segs []string
		for cur, n := id, 0; cur != "" && cur != "0" && n < 16; cur, n = parents[cur], n+1 {
			if nm, ok := names[cur]; ok {
				segs = append([]string{nm}, segs...)
			} else {
				break
			}
		}
		return strings.Join(segs, "/")
	}
	_ = s.mail.EnsureFolders(ctx, p.User)
	res := []mailFolder{}
	seen := map[string]bool{}
	add := func(id, name string, system bool) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		kind, custom := "", ""
		if system {
			kind, custom = systemKind(id)
		}
		if kind == "" {
			kind, custom = "custom", pathOf(id)
			if custom == "" {
				custom = name
			}
		}
		var fid uuid.UUID
		var err error
		if kind == "custom" {
			fid, err = s.mail.EnsureCustomFolder(ctx, p.User, custom)
		} else {
			err = s.pool.QueryRow(ctx,
				`SELECT id FROM mail_folders WHERE user_id=$1 AND kind=$2 LIMIT 1`, p.User, kind).Scan(&fid)
		}
		if err != nil {
			return err
		}
		res = append(res, mailFolder{ID: id, Name: name, Kind: kind, Target: fid})
		return nil
	}
	for _, f := range out.Items {
		if err := add(f.ID, f.Name, f.Type == 1); err != nil {
			return nil, err
		}
	}
	// INBOX/SENT are walked even if the folder list leaves them out.
	for _, id := range []string{"INBOX", "SENT"} {
		if err := add(id, id, true); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// StartMailFor queues one person's mail pass. full=true walks every page
// (first pass and the final pass); false is an incremental catch-up.
// 为某人排一轮邮件搬迁；full 为全量（首轮/收尾），否则增量。
func (s *Store) StartMailFor(ctx context.Context, openID string, full bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE feishu_people SET mail_status='queued', mail_error=NULL, mail_full=($2 OR mail_synced_at IS NULL)
		 WHERE open_id=$1 AND user_id IS NOT NULL AND email IS NOT NULL AND mail_status<>'running'`, openID, full)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM feishu_mail_cursors WHERE open_id=$1`, openID); err != nil {
		return err
	}
	var n int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM bg_tasks WHERE kind='feishu_mail' AND status IN ('queued','running') AND payload->>'open_id'=$1`,
		openID).Scan(&n)
	if n > 0 {
		return nil
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO bg_tasks (kind,payload) VALUES ('feishu_mail',$1)`,
		map[string]any{"open_id": openID})
	return err
}

// RunMail does one budgeted chunk of a person's mail pass; more=true asks
// the worker to continue. 一个预算块；more=true 表示还没完。
func (s *Store) RunMail(ctx context.Context, openID string, budget time.Duration) (bool, error) {
	deadline := time.Now().Add(budget)
	var p mailPerson
	var status string
	var uid *uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(email,''), user_id, mail_status, mail_full FROM feishu_people WHERE open_id=$1`, openID).
		Scan(&p.Mailbox, &uid, &status, &p.Full)
	if err != nil {
		return false, err
	}
	if status != "queued" && status != "running" {
		return false, nil
	}
	if uid == nil || p.Mailbox == "" {
		return false, s.mailErr(ctx, openID, "未关联 WittyChat 账号或缺少邮箱")
	}
	p.OpenID, p.User = openID, *uid
	c, err := s.appCreds(ctx)
	if err != nil {
		return false, s.mailErr(ctx, openID, err.Error())
	}
	if status == "queued" {
		_, _ = s.pool.Exec(ctx, `UPDATE feishu_people SET mail_status='running' WHERE open_id=$1`, openID)
	}
	folders, err := s.mailFolders(ctx, c, p)
	if err != nil {
		return false, s.mailFatal(ctx, openID, err)
	}
	if err := s.retryMailFailures(ctx, c, p, folders); err != nil {
		return false, s.mailFatal(ctx, openID, err)
	}
	for _, f := range folders {
		var page string
		var finished bool
		_ = s.pool.QueryRow(ctx,
			`SELECT page_token, finished FROM feishu_mail_cursors WHERE open_id=$1 AND folder_id=$2`,
			openID, f.ID).Scan(&page, &finished)
		for !finished {
			if time.Now().After(deadline) {
				return true, nil
			}
			var st string
			if err := s.pool.QueryRow(ctx, `SELECT mail_status FROM feishu_people WHERE open_id=$1`, openID).Scan(&st); err != nil || st != "running" {
				return false, nil
			}
			var out struct {
				Items     []string `json:"items"`
				HasMore   bool     `json:"has_more"`
				PageToken string   `json:"page_token"`
			}
			q := url.Values{"page_size": {"20"}, "folder_id": {f.ID}}
			if page != "" {
				q.Set("page_token", page)
			}
			if err := c.get(ctx, "/open-apis/mail/v1/user_mailboxes/"+url.PathEscape(p.Mailbox)+"/messages?"+q.Encode(), &out); err != nil {
				return false, s.mailFatal(ctx, openID, err)
			}
			fresh, err := s.unknownIDs(ctx, p, out.Items)
			if err != nil {
				return false, err
			}
			if len(fresh) > 0 {
				if err := s.importBatch(ctx, c, p, f, fresh); err != nil {
					return false, err
				}
			}
			skipped := len(out.Items) - len(fresh)
			if skipped > 0 {
				_, _ = s.pool.Exec(ctx, `UPDATE feishu_people SET mail_skipped=mail_skipped+$2 WHERE open_id=$1`, openID, skipped)
			}
			// Incremental pass: a full page we already have means the rest is
			// older mail we also have. 增量：整页都已导入即视为后面都有了。
			finished = !out.HasMore || out.PageToken == "" || (!p.Full && len(out.Items) > 0 && len(fresh) == 0)
			page = out.PageToken
			if _, err := s.pool.Exec(ctx,
				`INSERT INTO feishu_mail_cursors (open_id,folder_id,page_token,finished) VALUES ($1,$2,$3,$4)
				 ON CONFLICT (open_id,folder_id) DO UPDATE SET page_token=$3, finished=$4`,
				openID, f.ID, page, finished); err != nil {
				return false, err
			}
		}
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE feishu_people SET mail_status='done', mail_synced_at=now(), mail_full=false,
		  mail_failed=(SELECT count(*) FROM feishu_mail_failures WHERE open_id=$1)
		WHERE open_id=$1 AND mail_status='running'`, openID)
	if err == nil {
		_, err = s.pool.Exec(ctx, `DELETE FROM feishu_mail_cursors WHERE open_id=$1`, openID)
	}
	return false, err
}

func (s *Store) mailErr(ctx context.Context, openID, msg string) error {
	_, _ = s.pool.Exec(ctx, `UPDATE feishu_people SET mail_status='error', mail_error=$2 WHERE open_id=$1`, openID, msg)
	return nil
}

// mailFatal: permission problems end the person's pass with a readable
// reason (no retry); anything else is returned so the task retries.
// 权限问题：记原因结束（不重试）；其他错误交给任务重试。
func (s *Store) mailFatal(ctx context.Context, openID string, err error) error {
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrBadCreds) {
		return s.mailErr(ctx, openID, errMailboxDenied.Error()+"（"+err.Error()+"）")
	}
	return err
}

// unknownIDs filters out messages already imported for this person.
func (s *Store) unknownIDs(ctx context.Context, p mailPerson, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = mailKey(p.Mailbox, id)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT import_key FROM mail_messages WHERE owner_user_id=$1 AND import_key = ANY($2)`, p.User, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			have[k] = true
		}
	}
	var fresh []string
	for i, id := range ids {
		if !have[keys[i]] {
			fresh = append(fresh, id)
		}
	}
	return fresh, rows.Err()
}

func (s *Store) batchGet(ctx context.Context, c *fc, p mailPerson, ids []string) ([]fsMessage, error) {
	body, _ := json.Marshal(map[string]any{"format": "full", "message_ids": ids})
	var out struct {
		Messages []fsMessage `json:"messages"`
	}
	err := c.post(ctx, "/open-apis/mail/v1/user_mailboxes/"+url.PathEscape(p.Mailbox)+"/messages/batch_get", body, &out)
	return out.Messages, err
}

// importBatch fetches ≤20 messages and imports each; a per-message failure
// lands in the journal (retried next pass) and never stops the folder.
// 每批 ≤20 封；单封失败进台账（下一轮重试），不阻塞文件夹。
func (s *Store) importBatch(ctx context.Context, c *fc, p mailPerson, f mailFolder, ids []string) error {
	msgs, err := s.batchGet(ctx, c, p, ids)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			return s.mailFatal(ctx, p.OpenID, err)
		}
		return err
	}
	got := map[string]bool{}
	for i := range msgs {
		m := &msgs[i]
		got[m.MessageID] = true
		if err := s.importMessage(ctx, c, p, f, m); err != nil {
			s.recordMailFailure(ctx, p.OpenID, m.MessageID, f.ID, err)
			continue
		}
		_, _ = s.pool.Exec(ctx, `DELETE FROM feishu_mail_failures WHERE open_id=$1 AND message_id=$2`, p.OpenID, m.MessageID)
	}
	for _, id := range ids {
		if !got[id] {
			s.recordMailFailure(ctx, p.OpenID, id, f.ID, errors.New("飞书未返回该邮件详情"))
		}
	}
	return nil
}

func (s *Store) recordMailFailure(ctx context.Context, openID, msgID, folderID string, err error) {
	log.Printf("feishu mail %s %s: %v", openID, msgID, err)
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO feishu_mail_failures (open_id,message_id,folder_id,error) VALUES ($1,$2,$3,$4)
		ON CONFLICT (open_id,message_id) DO UPDATE SET error=$4, attempts=feishu_mail_failures.attempts+1, updated_at=now()`,
		openID, msgID, folderID, err.Error())
}

// retryMailFailures re-imports journaled messages at the start of a pass.
func (s *Store) retryMailFailures(ctx context.Context, c *fc, p mailPerson, folders []mailFolder) error {
	byID := map[string]mailFolder{}
	for _, f := range folders {
		byID[f.ID] = f
	}
	rows, err := s.pool.Query(ctx,
		`SELECT message_id, folder_id FROM feishu_mail_failures WHERE open_id=$1 ORDER BY updated_at LIMIT 200`, p.OpenID)
	if err != nil {
		return err
	}
	group := map[string][]string{}
	for rows.Next() {
		var id, fid string
		if rows.Scan(&id, &fid) == nil {
			group[fid] = append(group[fid], id)
		}
	}
	rows.Close()
	for fid, ids := range group {
		f, ok := byID[fid]
		if !ok {
			continue
		}
		for i := 0; i < len(ids); i += 20 {
			batch := ids[i:min(i+20, len(ids))]
			fresh, err := s.unknownIDs(ctx, p, batch)
			if err != nil {
				return err
			}
			if len(fresh) < len(batch) { // already imported meanwhile
				_, _ = s.pool.Exec(ctx, `DELETE FROM feishu_mail_failures WHERE open_id=$1 AND message_id = ANY($2)`,
					p.OpenID, batch)
			}
			if len(fresh) > 0 {
				if err := s.importBatch(ctx, c, p, f, fresh); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func hasLabel(ls []string, want string) bool {
	for _, l := range ls {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

func addrList(as []fsAddr) []string {
	var out []string
	for _, a := range as {
		if a.Address == "" {
			continue
		}
		if a.Name != "" {
			out = append(out, fmt.Sprintf("%s <%s>", a.Name, strings.ToLower(a.Address)))
		} else {
			out = append(out, strings.ToLower(a.Address))
		}
	}
	return out
}

// importMessage stores one Feishu message for the person.
func (s *Store) importMessage(ctx context.Context, c *fc, p mailPerson, f mailFolder, m *fsMessage) error {
	key := mailKey(p.Mailbox, m.MessageID)
	var received time.Time
	if ms, err := strconv.ParseInt(m.InternalDate, 10, 64); err == nil && ms > 0 {
		received = time.UnixMilli(ms)
	}
	opts := mail.DeliverOpts{
		IsRead:     !hasLabel(m.Labels, "UNREAD"),
		IsStarred:  hasLabel(m.Labels, "FLAGGED"),
		ReceivedAt: received,
		ImportKey:  key,
	}
	switch m.State {
	case 2, 3:
		opts.Direction = "out"
	default:
		opts.Direction = "in"
	}
	folder := f.Target
	if m.State == 3 && f.Kind == "inbox" { // a draft listed under a generic folder
		_ = s.pool.QueryRow(ctx, `SELECT id FROM mail_folders WHERE user_id=$1 AND kind='drafts' LIMIT 1`, p.User).Scan(&folder)
	}
	var parsed *mail.Parsed
	if m.Raw != "" {
		raw, err := b64url(m.Raw)
		if err != nil {
			return fmt.Errorf("raw 解码失败：%w", err)
		}
		obj, _, err := s.objects.Put(ctx, p.User, uuid.Nil, "mail_raw",
			"feishu-"+m.MessageID+".eml", "message/rfc822", bytes.NewReader(raw), int64(len(raw))+1)
		if err != nil {
			return err
		}
		opts.RawObjectID = &obj.ID
		opts.SizeBytes = int64(len(raw))
		parsed, err = mail.ParseMessage(bytes.NewReader(raw), func(meta mail.AttachmentMeta, r io.Reader) error {
			o, _, err := s.objects.Put(ctx, p.User, uuid.Nil, "mail", meta.Filename, meta.Mime, r, maxFileBytes)
			if err != nil {
				return err
			}
			opts.Attachments = append(opts.Attachments, mail.DeliveredAttachment{ObjectID: o.ID, ContentID: meta.ContentID, Inline: meta.Inline})
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		var err error
		if parsed, err = s.rebuild(ctx, c, p, m, &opts); err != nil {
			return err
		}
	}
	if parsed.Date.IsZero() {
		parsed.Date = received
	}
	opts.SentAt = parsed.Date
	// Same Message-ID already in this folder (e.g. delivered to WittyChat
	// after the MX switch) → skip. 同文件夹已有相同 Message-ID（切 MX 后已直投）则跳过。
	if parsed.MessageID != "" {
		var dup bool
		_ = s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM mail_messages WHERE owner_user_id=$1 AND folder_id=$2 AND rfc_msgid=$3)`,
			p.User, folder, parsed.MessageID).Scan(&dup)
		if dup {
			return nil
		}
	}
	if _, err := s.mail.DeliverParsed(ctx, p.User, "", &folder, parsed, opts); err != nil {
		if strings.Contains(err.Error(), "import_key") {
			return nil // raced with another pass
		}
		return err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE feishu_people SET mail_done=mail_done+1 WHERE open_id=$1`, p.OpenID)
	return nil
}

// rebuild turns Feishu's structured fields into a Parsed message and stores
// its attachments (inline body when present, otherwise the download URL).
// 按字段重建邮件；附件优先用内联 body，否则走下载链接。
func (s *Store) rebuild(ctx context.Context, c *fc, p mailPerson, m *fsMessage, opts *mail.DeliverOpts) (*mail.Parsed, error) {
	pr := &mail.Parsed{
		From:      strings.ToLower(m.From.Address),
		FromName:  m.From.Name,
		To:        addrList(m.To),
		Cc:        addrList(m.Cc),
		Bcc:       addrList(m.Bcc),
		Subject:   m.Subject,
		MessageID: strings.Trim(strings.TrimSpace(m.SMTPID), "<>"),
		InReplyTo: strings.Trim(strings.TrimSpace(m.InReplyTo), "<>"),
	}
	for _, r := range strings.Fields(m.References) {
		if r = strings.Trim(r, "<>"); r != "" {
			pr.References = append(pr.References, r)
		}
	}
	if b, err := b64url(m.HTML); err == nil {
		pr.HTML = string(b)
	}
	if b, err := b64url(m.Text); err == nil {
		pr.Text = string(b)
	}
	var need []fsAttachment
	for _, a := range m.Attachments {
		if a.Body != "" {
			data, err := b64url(a.Body)
			if err != nil {
				return nil, fmt.Errorf("附件 %s 解码失败", a.Filename)
			}
			if err := s.putAttachment(ctx, p, a, bytes.NewReader(data), opts); err != nil {
				return nil, err
			}
			continue
		}
		need = append(need, a)
	}
	if len(need) > 0 {
		urls, err := s.attachmentURLs(ctx, c, p, m.MessageID, need)
		if err != nil {
			return nil, err
		}
		for _, a := range need {
			u := urls[a.ID]
			if u == "" {
				return nil, fmt.Errorf("附件 %s 没有下载链接", a.Filename)
			}
			if err := s.fetchAttachment(ctx, p, a, u, opts); err != nil {
				return nil, err
			}
		}
	}
	return pr, nil
}

func (s *Store) putAttachment(ctx context.Context, p mailPerson, a fsAttachment, r io.Reader, opts *mail.DeliverOpts) error {
	name := a.Filename
	if name == "" {
		name = "attachment"
	}
	o, _, err := s.objects.Put(ctx, p.User, uuid.Nil, "mail", name, a.Mime, r, maxFileBytes)
	if err != nil {
		return err
	}
	opts.Attachments = append(opts.Attachments, mail.DeliveredAttachment{ObjectID: o.ID, ContentID: strings.Trim(a.CID, "<>"), Inline: a.Inline})
	return nil
}

func (s *Store) attachmentURLs(ctx context.Context, c *fc, p mailPerson, msgID string, atts []fsAttachment) (map[string]string, error) {
	res := map[string]string{}
	for i := 0; i < len(atts); i += 20 {
		q := url.Values{}
		for _, a := range atts[i:min(i+20, len(atts))] {
			q.Add("attachment_ids", a.ID)
		}
		var out struct {
			URLs []struct {
				ID  string `json:"attachment_id"`
				URL string `json:"download_url"`
			} `json:"download_urls"`
		}
		if err := c.get(ctx, "/open-apis/mail/v1/user_mailboxes/"+url.PathEscape(p.Mailbox)+"/messages/"+
			url.PathEscape(msgID)+"/attachments/download_url?"+q.Encode(), &out); err != nil {
			return nil, err
		}
		for _, u := range out.URLs {
			res[u.ID] = u.URL
		}
	}
	return res, nil
}

func (s *Store) fetchAttachment(ctx context.Context, p mailPerson, a fsAttachment, u string, opts *mail.DeliverOpts) error {
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "https" && (pu.Scheme != "http" || !isLoopbackHost(pu.Hostname()))) {
		return fmt.Errorf("附件 %s 下载地址不可信", a.Filename)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("附件 %s 下载失败：HTTP %d", a.Filename, resp.StatusCode)
	}
	if a.Mime == "" {
		a.Mime = resp.Header.Get("Content-Type")
	}
	return s.putAttachment(ctx, p, a, resp.Body, opts)
}

func isLoopbackHost(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }
