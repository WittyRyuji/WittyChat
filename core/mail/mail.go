// Package mail implements the mailbox model on top of the Phase-2 schema:
// addresses/aliases, folders, messages with FTS, attachments via objects.
// Internal recipients get direct delivery (no SMTP); external go through
// the worker's direct-to-MX sender.
//
// 内部收件人直投不走 SMTP；外部地址由 worker 直连 MX 发送。
package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("mail not found")
	ErrForbidden = errors.New("forbidden")
	ErrNoAddress = errors.New("user has no mail address / 用户未配置邮箱")
	ErrBadAddr   = errors.New("invalid address / 地址无效")
)

// LocalDomain decides whether a recipient is internal (direct delivery).
type Store struct {
	pool        *pgxpool.Pool
	LocalDomain string // e.g. "wittyloong.com"
}

func NewStore(pool *pgxpool.Pool, localDomain string) *Store {
	return &Store{pool: pool, LocalDomain: strings.ToLower(localDomain)}
}

type Address struct {
	ID      uuid.UUID `json:"id"`
	Address string    `json:"address"`
	Kind    string    `json:"kind"`
}

type Folder struct {
	ID       uuid.UUID `json:"id"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Position int       `json:"position"`
	Unread   int       `json:"unread"`
}

type Message struct {
	ID         uuid.UUID `json:"id"`
	FolderID   uuid.UUID `json:"folder_id"`
	Direction  string    `json:"direction"`
	ThreadID   string    `json:"thread_id"`
	FromAddr   string    `json:"from_addr"`
	ToAddrs    []string  `json:"to_addrs"`
	CcAddrs    []string  `json:"cc_addrs"`
	Subject    string    `json:"subject"`
	TextBody   string    `json:"text_body"`
	HTMLBody   string    `json:"html_body"`
	Preview    string    `json:"preview"`
	IsRead     bool      `json:"is_read"`
	IsStarred  bool      `json:"is_starred"`
	InReplyTo  string    `json:"in_reply_to,omitempty"`
	RFCMsgID   string    `json:"rfc_msgid,omitempty"`
	SentAt     time.Time `json:"sent_at"`
	ReceivedAt time.Time `json:"received_at"`
	Attach     []Attach  `json:"attachments,omitempty"`
}

type Attach struct {
	ObjectID  uuid.UUID `json:"object_id"`
	Name      string    `json:"name"`
	Mime      string    `json:"mime"`
	Size      int64     `json:"size_bytes"`
	W         *int      `json:"w,omitempty"` // decoded image px, if known / 图片像素宽（已知时）
	H         *int      `json:"h,omitempty"`
	ContentID *string   `json:"content_id,omitempty"` // cid: ref inside HTML / HTML 内 cid 引用
	Inline    bool      `json:"inline,omitempty"`
}

var defaultFolders = []struct{ kind, name string }{
	{"inbox", "Inbox"},
	{"sent", "Sent"},
	{"drafts", "Drafts"},
	{"spam", "Spam"},
	{"archive", "Archive"},
	{"trash", "Trash"},
}

// EnsureFolders lazily creates the six default folders for a user.
func (s *Store) EnsureFolders(ctx context.Context, user uuid.UUID) error {
	for i, f := range defaultFolders {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO mail_folders (id,user_id,kind,name,position) VALUES ($1,$2,$3,$4,$5)
			 ON CONFLICT (user_id,kind,name) DO NOTHING`,
			uuid.Must(uuid.NewV7()), user, f.kind, f.name, i); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Folders(ctx context.Context, user uuid.UUID) ([]Folder, error) {
	if err := s.EnsureFolders(ctx, user); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT f.id,f.kind,f.name,f.position,
		        (SELECT count(*) FROM mail_messages m
		          WHERE m.folder_id=f.id AND NOT m.is_read
		            AND m.direction='in') AS unread
		 FROM mail_folders f WHERE f.user_id=$1 ORDER BY f.position,f.name`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Folder{}
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.Kind, &f.Name, &f.Position, &f.Unread); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) folderID(ctx context.Context, user uuid.UUID, kind string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM mail_folders WHERE user_id=$1 AND kind=$2 ORDER BY position LIMIT 1`,
		user, kind).Scan(&id)
	return id, err
}

// PrimaryAddress returns the user's primary address (first one if none marked).
func (s *Store) PrimaryAddress(ctx context.Context, user uuid.UUID) (string, error) {
	var addr string
	err := s.pool.QueryRow(ctx,
		`SELECT address FROM mail_addresses WHERE user_id=$1 ORDER BY kind='primary' DESC, created_at LIMIT 1`,
		user).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoAddress
	}
	return addr, err
}

func (s *Store) Addresses(ctx context.Context, user uuid.UUID) ([]Address, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id,address,kind FROM mail_addresses WHERE user_id=$1 ORDER BY created_at`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Address{}
	for rows.Next() {
		var a Address
		if err := rows.Scan(&a.ID, &a.Address, &a.Kind); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddAddress(ctx context.Context, user uuid.UUID, addr, kind string) error {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if !strings.Contains(addr, "@") || kind == "" {
		return ErrBadAddr
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO mail_addresses (id,user_id,address,kind) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (address) DO NOTHING`,
		uuid.Must(uuid.NewV7()), user, addr, kind)
	return err
}

func (s *Store) RemoveAddress(ctx context.Context, user, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM mail_addresses WHERE id=$1 AND user_id=$2 AND kind='alias'`, id, user)
	return err
}

// UserForAddress resolves an internal recipient address → user id.
// Falls back to the login email so recipients who never opened mail
// still get direct delivery (address is backfilled on first lookup).
// UserForAddress 解析内部收件人。若对方从未用过邮件，
// 按其登录邮箱兜底并顺手补配主地址，保证内部直投可达。
func (s *Store) UserForAddress(ctx context.Context, addr string) (uuid.UUID, error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	var uid uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM mail_addresses WHERE address=$1`, addr).Scan(&uid)
	if err == nil {
		return uid, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE lower(email)=$1 AND status='active'`, addr).Scan(&uid); err != nil {
		return uuid.Nil, err
	}
	_ = s.AddAddress(ctx, uid, addr, "primary")
	_ = s.EnsureFolders(ctx, uid)
	return uid, nil
}

func (s *Store) IsLocal(addr string) bool {
	i := strings.LastIndex(addr, "@")
	return i > 0 && s.LocalDomain != "" &&
		strings.EqualFold(addr[i+1:], s.LocalDomain)
}

const msgCols = `id, folder_id, direction, COALESCE(thread_id,''), from_addr,
	to_addrs, cc_addrs, subject, text_body, html_body, preview, is_read, is_starred,
	COALESCE(in_reply_to,''), COALESCE(rfc_msgid,''), COALESCE(sent_at, received_at), received_at`

func scanMsg(row pgx.Row) (*Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.FolderID, &m.Direction, &m.ThreadID, &m.FromAddr,
		&m.ToAddrs, &m.CcAddrs, &m.Subject, &m.TextBody, &m.HTMLBody, &m.Preview,
		&m.IsRead, &m.IsStarred, &m.InReplyTo, &m.RFCMsgID, &m.SentAt, &m.ReceivedAt)
	return &m, err
}

func (s *Store) List(ctx context.Context, user, folder uuid.UUID, limit, off int) ([]Message, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+msgCols+` FROM mail_messages
		 WHERE owner_user_id=$1 AND folder_id=$2
		 ORDER BY received_at DESC LIMIT $3 OFFSET $4`, user, folder, limit, off)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, user, id uuid.UUID) (*Message, error) {
	m, err := scanMsg(s.pool.QueryRow(ctx,
		`SELECT `+msgCols+` FROM mail_messages WHERE id=$1 AND owner_user_id=$2`, id, user))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// attachments
	rows, err := s.pool.Query(ctx,
		`SELECT a.object_id,a.name,COALESCE(a.mime,''),a.size_bytes,o.width,o.height,a.content_id,a.inline
		 FROM mail_attachments a LEFT JOIN objects o ON o.id=a.object_id
		 WHERE a.message_id=$1`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var a Attach
			if rows.Scan(&a.ObjectID, &a.Name, &a.Mime, &a.Size, &a.W, &a.H, &a.ContentID, &a.Inline) == nil {
				m.Attach = append(m.Attach, a)
			}
		}
	}
	return m, nil
}

func (s *Store) Search(ctx context.Context, user uuid.UUID, q string, limit int) ([]Message, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+msgCols+` FROM mail_messages
		 WHERE owner_user_id=$1 AND (search_text ILIKE '%'||$2||'%' OR search_tsv @@ plainto_tsquery('simple',$2))
		 ORDER BY received_at DESC LIMIT $3`, user, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *Store) SetFlags(ctx context.Context, user, id uuid.UUID, read, starred *bool) error {
	if read != nil {
		if _, err := s.pool.Exec(ctx,
			`UPDATE mail_messages SET is_read=$1 WHERE id=$2 AND owner_user_id=$3`,
			*read, id, user); err != nil {
			return err
		}
	}
	if starred != nil {
		if _, err := s.pool.Exec(ctx,
			`UPDATE mail_messages SET is_starred=$1 WHERE id=$2 AND owner_user_id=$3`,
			*starred, id, user); err != nil {
			return err
		}
	}
	return nil
}

// MarkAllRead marks every unread message in one of the user's folders.
// 把某文件夹内未读邮件一次全部置为已读。
func (s *Store) MarkAllRead(ctx context.Context, user, folder uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE mail_messages SET is_read=true
		 WHERE owner_user_id=$1 AND folder_id=$2 AND NOT is_read`,
		user, folder)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) Move(ctx context.Context, user, id, folder uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE mail_messages SET folder_id=$1 WHERE id=$2 AND owner_user_id=$3`,
		folder, id, user)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// Delete removes the user's own mailbox copy and its attachment links
// (object files are shared and stay). 删除本人邮箱副本及其附件关联（对象文件不动）。
func (s *Store) Delete(ctx context.Context, user, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM mail_attachments WHERE message_id IN
			 (SELECT id FROM mail_messages WHERE id=$1 AND owner_user_id=$2)`, id, user); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM mail_messages WHERE id=$1 AND owner_user_id=$2`, id, user)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
}

func preview(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if len(body) > 140 {
		return body[:140]
	}
	return body
}

// Deliver inserts a mailbox copy for one local user. attachments are
// object ids already uploaded to the object store.
func (s *Store) Deliver(ctx context.Context, toUser uuid.UUID, folder string,
	fromAddr string, to, cc []string, subject, body, rfcID, inReplyTo string,
	attachIDs []uuid.UUID,
) (uuid.UUID, error) {
	fid, err := s.folderID(ctx, toUser, folder)
	if err != nil {
		return uuid.Nil, err
	}
	if to == nil {
		to = []string{}
	}
	if cc == nil {
		cc = []string{}
	}
	thread := inReplyTo
	if thread == "" {
		thread = normalizeSubject(subject)
	}
	attNames := []string{}
	for _, oid := range attachIDs {
		var n string
		if err := s.pool.QueryRow(ctx, `SELECT name FROM objects WHERE id=$1`, oid).Scan(&n); err == nil {
			attNames = append(attNames, n)
		}
	}
	search := strings.Join([]string{
		subject, body, fromAddr,
		strings.Join(to, " "), strings.Join(attNames, " "),
	}, " ")
	id := uuid.Must(uuid.NewV7())
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO mail_messages
			 (id,owner_user_id,folder_id,direction,rfc_msgid,thread_id,in_reply_to,
			  from_addr,to_addrs,cc_addrs,subject,text_body,preview,search_text,is_read,sent_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			id, toUser, fid, or(dir(folder)), nilable(rfcID), thread, nilable(inReplyTo),
			fromAddr, to, cc, subject, body, preview(body), search,
			folder == "sent", // own sent copy never shows as unread / 发件箱副本默认已读
			time.Now()); err != nil {
			return err
		}
		for _, oid := range attachIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO mail_attachments (message_id,object_id,name,size_bytes,mime)
				 SELECT $1,id,name,size_bytes,mime FROM objects WHERE id=$2`, id, oid); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

func dir(folder string) string {
	if folder == "sent" {
		return "out"
	}
	return "in"
}
func or(s string) string { return s }

func nilable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func normalizeSubject(subj string) string {
	s := strings.ToLower(strings.TrimSpace(subj))
	for strings.HasPrefix(s, "re:") || strings.HasPrefix(s, "fwd:") || strings.HasPrefix(s, "回复:") {
		i := strings.Index(s, ":")
		s = strings.TrimSpace(s[i+1:])
	}
	return s
}

// Compose is one outbound attempt: local recipients get direct Deliver;
// external ones are returned for the SMTP path.
func (s *Store) Compose(ctx context.Context, fromUser uuid.UUID,
	to, cc []string, subject, body, inReplyTo string, attachIDs []uuid.UUID,
) (msgID uuid.UUID, external []string, err error) {
	fromAddr, err := s.PrimaryAddress(ctx, fromUser)
	if err != nil {
		return uuid.Nil, nil, err
	}
	if len(to) == 0 {
		return uuid.Nil, nil, ErrBadAddr
	}
	// Sender's own copy goes to Sent.
	rfcID := fmt.Sprintf("<%d@wittychat>", time.Now().UnixNano())
	msgID, err = s.Deliver(ctx, fromUser, "sent", fromAddr, to, cc,
		subject, body, rfcID, inReplyTo, attachIDs)
	if err != nil {
		return uuid.Nil, nil, err
	}
	for _, rcpt := range append(append([]string{}, to...), cc...) {
		rcpt = strings.ToLower(strings.TrimSpace(rcpt))
		if s.IsLocal(rcpt) {
			if uid, err := s.UserForAddress(ctx, rcpt); err == nil {
				_, _ = s.Deliver(ctx, uid, "inbox", fromAddr, to, cc,
					subject, body, rfcID, inReplyTo, attachIDs)
				continue
			}
		}
		external = append(external, rcpt)
	}
	return msgID, external, nil
}

// DeliveredAttachment pairs an already-stored object with its mail role —
// inline images keep their content-id so the client can wire cid: refs.
// 已入库对象与其邮件角色：内联图片保留 content-id 供客户端接 cid: 引用。
type DeliveredAttachment struct {
	ObjectID  uuid.UUID
	ContentID string
	Inline    bool
}

// DeliverOpts carries the inbound-specific fields DeliverParsed needs —
// zero times fall back to now; an empty Direction derives from folder.
// 入站专属字段：零值时间回退为当前；空方向按文件夹推断。
type DeliverOpts struct {
	RawObjectID *uuid.UUID
	Attachments []DeliveredAttachment
	IsRead      bool
	IsStarred   bool
	SentAt      time.Time
	ReceivedAt  time.Time
	Direction   string // "" → derived from folder
	SizeBytes   int64
	ImportKey   string // mailbox-import dedup key / 导入去重键
}

// DeliverParsed stores one mailbox copy of a fully parsed message.
// folderID non-nil targets that exact folder (custom folders for imports);
// otherwise folderKind resolves through the kind folders.
// 存一份完整解析后的邮件：folderID 指定精确文件夹（导入用自定义夹），
// 否则按 kind 文件夹解析。
func (s *Store) DeliverParsed(ctx context.Context, toUser uuid.UUID, folderKind string,
	folderID *uuid.UUID, p *Parsed, o DeliverOpts,
) (uuid.UUID, error) {
	var fid uuid.UUID
	var err error
	if folderID != nil {
		fid = *folderID
	} else {
		fid, err = s.folderID(ctx, toUser, folderKind)
	}
	if err != nil {
		return uuid.Nil, err
	}
	if p.To == nil {
		p.To = []string{}
	}
	if p.Cc == nil {
		p.Cc = []string{}
	}
	direction := o.Direction
	if direction == "" {
		direction = dir(folderKind)
	}
	thread := p.InReplyTo
	if thread == "" {
		thread = normalizeSubject(p.Subject)
	}
	text := p.Text
	attNames := []string{}
	for _, a := range o.Attachments {
		var n string
		if err := s.pool.QueryRow(ctx, `SELECT name FROM objects WHERE id=$1`, a.ObjectID).Scan(&n); err == nil {
			attNames = append(attNames, n)
		}
	}
	search := strings.Join([]string{
		p.Subject, text, p.From, strings.Join(p.To, " "), strings.Join(attNames, " "),
	}, " ")
	sentAt := o.SentAt
	if sentAt.IsZero() {
		sentAt = time.Now()
	}
	receivedAt := o.ReceivedAt
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	id := uuid.Must(uuid.NewV7())
	// Postgres rejects 0x00/invalid UTF-8 in text columns — real-world mail
	// decodes to both sometimes; sanitize every text field we insert.
	// PG 文本列拒 0x00/非法 UTF-8——真实邮件解码后都会有，入库前全部净化。
	clean := func(v string) string {
		return strings.ReplaceAll(strings.ToValidUTF8(v, "�"), "\x00", "")
	}
	cleanList := func(vs []string) []string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = clean(v)
		}
		return out
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO mail_messages
			 (id,owner_user_id,folder_id,direction,rfc_msgid,thread_id,in_reply_to,
			  from_addr,to_addrs,cc_addrs,subject,text_body,html_body,preview,search_text,
			  is_read,is_starred,sent_at,received_at,raw_object_id,size_bytes,import_key)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
			id, toUser, fid, direction, nilable(clean(p.MessageID)), clean(thread), nilable(clean(p.InReplyTo)),
			clean(p.From), cleanList(p.To), cleanList(p.Cc), clean(p.Subject),
			clean(text), clean(p.HTML), preview(clean(text)), clean(search),
			o.IsRead, o.IsStarred, sentAt, receivedAt, o.RawObjectID, o.SizeBytes, nilable(o.ImportKey)); err != nil {
			return err
		}
		for _, a := range o.Attachments {
			if _, err := tx.Exec(ctx,
				`INSERT INTO mail_attachments (message_id,object_id,name,size_bytes,mime,content_id,inline)
				 SELECT $1,id,name,size_bytes,mime,$3,$4 FROM objects WHERE id=$2`,
				id, a.ObjectID, nilable(a.ContentID), a.Inline); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}

// EnsureCustomFolder returns (creating if needed) a per-user folder of
// kind 'custom' — used by mailbox import for provider-side folders.
// 按名取（或建）用户的 custom 文件夹——邮箱导入映射来源文件夹用。
func (s *Store) EnsureCustomFolder(ctx context.Context, user uuid.UUID, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO mail_folders (id, user_id, kind, name)
		 VALUES ($1,$2,'custom',$3)
		 ON CONFLICT (user_id, kind, name) DO UPDATE SET name=EXCLUDED.name
		 RETURNING id`, uuid.Must(uuid.NewV7()), user, name).Scan(&id)
	return id, err
}
