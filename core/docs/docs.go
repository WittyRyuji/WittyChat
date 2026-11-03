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
)

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

// ListByConv lists docs bound or shared to a conversation (群文档面板).
func (s *Store) ListByConv(ctx context.Context, convID uuid.UUID) ([]Doc, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+docCols+` FROM docs d
		WHERE d.deleted_at IS NULL
		  AND (d.conv_id=$1 OR EXISTS (SELECT 1 FROM doc_shares sh
		       WHERE sh.doc_id=d.id AND sh.conv_id=$1))
		ORDER BY d.updated_at DESC`, convID)
	if err != nil {
		return nil, err
	}
	return scanDocs(rows)
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
// growth is bounded by Compact (client snapshots), not by byte dedupe.
// 追加一条 Yjs update；日志增长由 Compact（客户端快照）收敛。
func (s *Store) AppendUpdate(ctx context.Context, docID uuid.UUID, data []byte) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE docs SET updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, docID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `INSERT INTO doc_updates (doc_id, data) VALUES ($1,$2)`, docID, data)
		return err
	})
}

// LoadUpdates replays the full update log; maxSeq is the highest seq
// returned (0 when empty) — the compaction watermark for this replay.
// 回放全部 update；maxSeq 是本次回放的最高序号，作为压缩水位。
func (s *Store) LoadUpdates(ctx context.Context, docID uuid.UUID) (out [][]byte, maxSeq int64, err error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, data FROM doc_updates WHERE doc_id=$1 ORDER BY seq`, docID)
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

// Compact replaces every update with seq <= upTo by one snapshot. Safe
// because the snapshot is a client's full state captured AFTER it applied
// the replay that ended at upTo (y-protocols process frames in order), so
// it is a superset of those updates; later updates are untouched.
// 压缩：用一个快照替换 seq<=upTo 的全部 update。快照是客户端在应用完截至
// upTo 的回放之后的全量状态（协议按序处理），必为其超集；更晚的不动。
func (s *Store) Compact(ctx context.Context, docID uuid.UUID, snapshot []byte, upTo int64) error {
	if len(snapshot) == 0 || upTo <= 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM doc_updates WHERE doc_id=$1 AND seq <= $2`, docID, upTo); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO doc_updates (doc_id, data) VALUES ($1,$2)`, docID, snapshot)
		return err
	})
}
