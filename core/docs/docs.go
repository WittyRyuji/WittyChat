// Package docs implements native collaborative documents (Feishu-style):
// a doc binds optionally to a conversation so conv members co-edit in
// realtime, and can be shared into further conversations. Content is a
// Yjs update log the WS relay appends to and replays for late joiners —
// the store itself never parses updates.
//
// 原生协同文档（飞书式）：可绑定会话实现成员实时共编，也可分享到其它
// 会话。内容是 Yjs update 日志，WS 中继追加落库、给后进者回放——本包不
// 解析帧内容。
package docs

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("docs: not found")
	ErrBadKind  = errors.New("docs: bad kind")
	ErrLogFull  = errors.New("docs: update log over the workspace cap")
)

// compactMinRows is the smallest log worth compacting — below it the
// rewrite cost buys nothing and only adds risk.
// 日志少于此行数不压缩——重写代价换不来收益，只引入风险。
const compactMinRows = 50

// MaxTitleRunes caps doc titles. 标题长度上限。
const MaxTitleRunes = 200

type Doc struct {
	ID        uuid.UUID  `json:"id"`
	Title     string     `json:"title"`
	Kind      string     `json:"kind"` // doc | sheet | slide
	Owner     uuid.UUID  `json:"owner"`
	ConvID    *uuid.UUID `json:"conv_id"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Editors   int        `json:"editors"` // live editors, filled by relay
	Shared    bool       `json:"shared"`  // ListByConv: here via doc_shares, not the binding / 经分享而非绑定
}

type Store struct {
	pool         *pgxpool.Pool
	isConvMember func(ctx context.Context, convID, uid uuid.UUID) bool
	convRole     func(ctx context.Context, convID, uid uuid.UUID) (string, error)
}

// New wires the store. isConvMember/convRole are supplied by httpapi
// (im store) so conv-bound doc ACL stays consistent with membership.
// isConvMember/convRole 由 httpapi 注入（im store），保证绑定文档的
// 权限与会话成员一致。
func New(pool *pgxpool.Pool,
	isConvMember func(ctx context.Context, convID, uid uuid.UUID) bool,
	convRole func(ctx context.Context, convID, uid uuid.UUID) (string, error),
) *Store {
	return &Store{pool: pool, isConvMember: isConvMember, convRole: convRole}
}

// ValidKind guards create/update kind values. 合法文档类型。
func ValidKind(k string) bool {
	return k == "doc" || k == "sheet" || k == "slide"
}

// NormTitle trims and caps a title (rune-safe). 标题去空白并按字符截断。
func NormTitle(t string) string {
	t = strings.TrimSpace(t)
	if utf8.RuneCountInString(t) > MaxTitleRunes {
		t = string([]rune(t)[:MaxTitleRunes])
	}
	return t
}

const docCols = `d.id, d.title, d.kind, d.owner, d.conv_id, d.created_at, d.updated_at`

func scanDocs(rows pgx.Rows) ([]Doc, error) {
	defer rows.Close()
	out := []Doc{}
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.ID, &d.Title, &d.Kind, &d.Owner, &d.ConvID, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Create makes a doc owned by uid, optionally bound to a conv.
func (s *Store) Create(ctx context.Context, uid uuid.UUID, title, kind string, convID *uuid.UUID) (*Doc, error) {
	if !ValidKind(kind) {
		return nil, ErrBadKind
	}
	if convID != nil && !s.isConvMember(ctx, *convID, uid) {
		return nil, ErrNotFound
	}
	d := &Doc{ID: uuid.Must(uuid.NewV7()), Title: NormTitle(title), Kind: kind, Owner: uid, ConvID: convID}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO docs (id, title, kind, owner, conv_id) VALUES ($1,$2,$3,$4,$5)
		 RETURNING created_at, updated_at`,
		d.ID, d.Title, kind, uid, convID).Scan(&d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// CanManage: owner always; conv-bound docs also by conv owner/admin
// (delete/rename/share rights match group management). 管理权=属主，或
// 绑定会话的群主/管理员。
func (s *Store) CanManage(ctx context.Context, docID, uid uuid.UUID) bool {
	var owner uuid.UUID
	var convID *uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT owner, conv_id FROM docs WHERE id=$1 AND deleted_at IS NULL`, docID).
		Scan(&owner, &convID)
	if err != nil {
		return false
	}
	if owner == uid {
		return true
	}
	if convID == nil {
		return false
	}
	role, err := s.convRole(ctx, *convID, uid)
	return err == nil && (role == "owner" || role == "admin")
}

// Delete soft-deletes: invisible to everyone, history kept for audit.
// 软删除：对所有人不可见，历史保留供审计。
func (s *Store) Delete(ctx context.Context, docID, by uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE docs SET deleted_at=now(), deleted_by=$2 WHERE id=$1 AND deleted_at IS NULL`,
		docID, by)
	return err
}

// Share grants a conversation's members access (doc link sent there).
// 分享到会话：该会话成员获得访问权。
func (s *Store) Share(ctx context.Context, docID, convID, by uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO doc_shares (doc_id, conv_id, shared_by) VALUES ($1,$2,$3)
		 ON CONFLICT DO NOTHING`, docID, convID, by)
	return err
}

// AccessibleToConv: doc is bound or shared to convID. 文档对该会话可见。
func (s *Store) AccessibleToConv(ctx context.Context, docID, convID uuid.UUID) bool {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM docs d WHERE d.id=$1 AND d.deleted_at IS NULL
		  AND (d.conv_id=$2 OR EXISTS (SELECT 1 FROM doc_shares sh
		       WHERE sh.doc_id=d.id AND sh.conv_id=$2)))`, docID, convID).Scan(&ok)
	return err == nil && ok
}

// ListByConv lists docs bound or shared to a conversation (群文档面板);
// Shared marks the ones present only through doc_shares.
func (s *Store) ListByConv(ctx context.Context, convID uuid.UUID) ([]Doc, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+docCols+`,
		       EXISTS (SELECT 1 FROM doc_shares sh2
		               WHERE sh2.doc_id=d.id AND sh2.conv_id=$1) AS shared
		FROM docs d
		WHERE d.deleted_at IS NULL
		  AND (d.conv_id=$1 OR EXISTS (SELECT 1 FROM doc_shares sh
		       WHERE sh.doc_id=d.id AND sh.conv_id=$1))
		ORDER BY d.updated_at DESC`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Doc{}
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.ID, &d.Title, &d.Kind, &d.Owner, &d.ConvID,
			&d.CreatedAt, &d.UpdatedAt, &d.Shared); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// accessSQL: owner, member of the bound conv, or member of a conv the doc
// was shared into ($1=doc row alias d, $uid placeholder supplied by caller).
// 访问条件：属主 / 绑定会话成员 / 分享会话成员。
const accessSQL = `(d.owner = $UID
	OR EXISTS (SELECT 1 FROM conv_members m
	           WHERE m.conv_id = d.conv_id AND m.user_id = $UID AND m.left_at IS NULL)
	OR EXISTS (SELECT 1 FROM doc_shares sh JOIN conv_members m
	             ON m.conv_id = sh.conv_id AND m.user_id = $UID AND m.left_at IS NULL
	           WHERE sh.doc_id = d.id))`

func access(placeholder string) string { return strings.ReplaceAll(accessSQL, "$UID", placeholder) }

// CanAccess: owner, bound-conv member, or shared-conv member (live docs).
// 访问=属主 / 绑定会话成员 / 分享会话成员（未删除文档）。
func (s *Store) CanAccess(ctx context.Context, docID, uid uuid.UUID) bool {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM docs d WHERE d.id=$1 AND d.deleted_at IS NULL AND `+
			access("$2")+`)`, docID, uid).Scan(&ok)
	return err == nil && ok
}

func (s *Store) Get(ctx context.Context, docID uuid.UUID) (*Doc, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+docCols+` FROM docs d WHERE d.id=$1 AND d.deleted_at IS NULL`, docID)
	if err != nil {
		return nil, err
	}
	list, err := scanDocs(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// List returns docs the user can access: own + bound/shared conv docs.
func (s *Store) List(ctx context.Context, uid uuid.UUID) ([]Doc, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+docCols+` FROM docs d WHERE d.deleted_at IS NULL AND `+access("$1")+
			` ORDER BY d.updated_at DESC`, uid)
	if err != nil {
		return nil, err
	}
	return scanDocs(rows)
}

func (s *Store) Rename(ctx context.Context, docID uuid.UUID, title string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE docs SET title=$2, updated_at=now() WHERE id=$1 AND deleted_at IS NULL`,
		docID, NormTitle(title))
	return err
}

// AppendUpdate persists one Yjs update blob and bumps updated_at. Log
// growth is bounded by Compact (owner snapshots) and capped by
// maxLogBytes (docs.log_max_mb) so a member can't fill the DB.
// 追加一条 Yjs update；日志由属主快照压缩收敛，并受 maxLogBytes
// （docs.log_max_mb）限制，成员无法无限写库。
func (s *Store) AppendUpdate(ctx context.Context, docID uuid.UUID, data []byte, maxLogBytes int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// One UPDATE takes the doc row lock (serializing appends for this
		// doc), bumps the running log_bytes total in the same write, and
		// hands it back — the cap check costs O(1) instead of a full-log
		// SUM per keystroke. 一次 UPDATE 拿文档行锁（串行化本文档的追加）、
		// 顺带维护 log_bytes 计数并返回——上限检查 O(1)，不再逐键击全表 SUM。
		var logBytes int64
		if err := tx.QueryRow(ctx,
			`UPDATE docs SET log_bytes = log_bytes + $2, updated_at = now()
			 WHERE id=$1 AND deleted_at IS NULL RETURNING log_bytes`,
			docID, int64(len(data))).Scan(&logBytes); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if maxLogBytes > 0 && logBytes > maxLogBytes {
			return ErrLogFull
		}
		_, err := tx.Exec(ctx, `INSERT INTO doc_updates (doc_id, data) VALUES ($1,$2)`, docID, data)
		return err
	})
}

// LoadUpdates replays the log after `after` seq (0 = full log); maxSeq is
// the highest seq returned (0 when empty) — the compaction watermark.
// 回放 seq > after 的 update（0=全量）；maxSeq 为本次回放最高序号。
func (s *Store) LoadUpdates(ctx context.Context, docID uuid.UUID, after int64) (out [][]byte, maxSeq int64, err error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, data FROM doc_updates WHERE doc_id=$1 AND seq>$2 ORDER BY seq`, docID, after)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			return nil, 0, err
		}
		out = append(out, b)
		maxSeq = seq
	}
	return out, maxSeq, rows.Err()
}

// CompactIfLarge replaces every update with seq <= upTo by one snapshot —
// only when the log is at least compactMinRows long OR already at the
// byte cap (a short-but-fat log must still compact, otherwise a doc over
// docs.log_max_mb can never recover). Replaced rows are archived (not
// deleted) for the audit retention window. The caller restricts this to
// the doc owner's connection: a member must not be able to swap the
// whole history (see the a460bd3 review).
// 压缩：用一个快照替换 seq<=upTo 的全部 update——行数达阈值或日志已到
// 字节上限时执行（行少但字节肥的日志也必须能压，否则超过
// docs.log_max_mb 的文档永远无法恢复）。被替换的行归档保留至审计期。
// 调用方限定只有属主连接可触发：普通成员不得整体改写历史（见 a460bd3 审查）。
func (s *Store) CompactIfLarge(ctx context.Context, docID uuid.UUID, snapshot []byte, upTo, minBytes int64, force bool) (bool, error) {
	if len(snapshot) == 0 || upTo <= 0 {
		return false, nil
	}
	var done bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var n int
		var logBytes int64
		if err := tx.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM doc_updates WHERE doc_id=$1 AND seq <= $2),
			        COALESCE((SELECT log_bytes FROM docs WHERE id=$1), 0)`,
			docID, upTo).Scan(&n, &logBytes); err != nil {
			return err
		}
		// force = the room saw ErrLogFull: log_bytes never actually reaches
		// the cap (the tipping append rolls back), so a byte comparison
		// alone would leave a "full" doc stuck forever. / force=房间见过
		// ErrLogFull：log_bytes 永远到不了上限（越限的追加会回滚），只按
		// 字节比较会让"写满"的文档永远卡死。
		if !force && n < compactMinRows && (minBytes <= 0 || logBytes < minBytes) {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO doc_update_archive (doc_id, seq, data)
			 SELECT doc_id, seq, data FROM doc_updates WHERE doc_id=$1 AND seq <= $2`,
			docID, upTo); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM doc_updates WHERE doc_id=$1 AND seq <= $2`, docID, upTo); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO doc_updates (doc_id, data) VALUES ($1,$2)`, docID, snapshot); err != nil {
			return err
		}
		// Recompute the byte counter over what remains (snapshot + any
		// rows past upTo). / 重算剩余日志字节数（快照 + upTo 之后的行）。
		if _, err := tx.Exec(ctx,
			`UPDATE docs SET log_bytes = (
				SELECT COALESCE(SUM(LENGTH(u.data)),0) FROM doc_updates u WHERE u.doc_id=$1)
			 WHERE id=$1`, docID); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done, err
}

// OwnerOf returns the doc's owner id (for owner-only compaction).
// 返回文档属主 id（压缩仅属主连接可触发）。
func (s *Store) OwnerOf(ctx context.Context, docID uuid.UUID) (uuid.UUID, error) {
	var owner uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT owner FROM docs WHERE id=$1 AND deleted_at IS NULL`, docID).Scan(&owner)
	return owner, err
}

// Unshare removes a doc_shares grant. 撤销一次分享。
func (s *Store) Unshare(ctx context.Context, docID, convID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM doc_shares WHERE doc_id=$1 AND conv_id=$2`, docID, convID)
	return err
}

// PurgeArchive deletes archived update rows past the audit-retention
// window — archive is for audit/recovery, not forever. Package-level so
// the worker can run it without the member callbacks.
// 归档清除：删除超过审计保留期的归档行——归档为审计/恢复，非永久保留。
// 包级函数，worker 无需成员回调即可执行。
func PurgeArchive(ctx context.Context, pool *pgxpool.Pool, older time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM doc_update_archive WHERE compacted_at < $1`,
		time.Now().Add(-older))
	return tag.RowsAffected(), err
}
