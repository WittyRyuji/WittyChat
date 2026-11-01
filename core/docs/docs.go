// Package docs implements native collaborative documents (Feishu-style):
// a doc binds optionally to a conversation so conv members co-edit in
// realtime. Content is a Yjs update log the WS relay appends to and
// replays for late joiners — the store itself never parses updates.
//
// 原生协同文档（飞书式）：可绑定会话实现成员实时共编。内容是 Yjs
// update 日志，WS 中继追加落库、给后进者回放——本包不解析帧内容。
package docs

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("docs: not found")

type Doc struct {
	ID        uuid.UUID  `json:"id"`
	Title     string     `json:"title"`
	Owner     uuid.UUID  `json:"owner"`
	ConvID    *uuid.UUID `json:"conv_id"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Editors   int        `json:"editors"` // live editors, filled by relay
}

type Store struct {
	pool         *pgxpool.Pool
	isConvMember func(ctx context.Context, convID, uid uuid.UUID) bool
}

// New wires the store. isConvMember is supplied by httpapi (im store) so
// conv-bound doc ACL stays consistent with conversation membership.
// isConvMember 由 httpapi 注入（im store），保证绑定文档的权限与会话
// 成员一致。
func New(pool *pgxpool.Pool, isConvMember func(ctx context.Context, convID, uid uuid.UUID) bool) *Store {
	return &Store{pool: pool, isConvMember: isConvMember}
}

// Create makes a doc owned by uid, optionally bound to a conv.
func (s *Store) Create(ctx context.Context, uid uuid.UUID, title string, convID *uuid.UUID) (*Doc, error) {
	if convID != nil && !s.isConvMember(ctx, *convID, uid) {
		return nil, ErrNotFound
	}
	d := &Doc{ID: uuid.New(), Title: title, Owner: uid, ConvID: convID}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO docs (id, title, owner, conv_id) VALUES ($1,$2,$3,$4)`,
		d.ID, title, uid, convID)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// CanAccess: owner, or member of the bound conv. 访问=属主或绑定会话成员。
func (s *Store) CanAccess(ctx context.Context, docID, uid uuid.UUID) bool {
	var owner uuid.UUID
	var convID *uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT owner, conv_id FROM docs WHERE id=$1`, docID).
		Scan(&owner, &convID)
	if err != nil {
		return false
	}
	if owner == uid {
		return true
	}
	return convID != nil && s.isConvMember(ctx, *convID, uid)
}

func (s *Store) Get(ctx context.Context, docID uuid.UUID) (*Doc, error) {
	var d Doc
	err := s.pool.QueryRow(ctx,
		`SELECT id, title, owner, conv_id, created_at, updated_at FROM docs WHERE id=$1`, docID).
		Scan(&d.ID, &d.Title, &d.Owner, &d.ConvID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

// List returns docs the user can access: own + docs bound to their convs.
func (s *Store) List(ctx context.Context, uid uuid.UUID) ([]Doc, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.title, d.owner, d.conv_id, d.created_at, d.updated_at
		FROM docs d
		WHERE d.owner = $1
		   OR (d.conv_id IS NOT NULL AND EXISTS (
		       SELECT 1 FROM conv_members m
		       WHERE m.conv_id = d.conv_id AND m.user_id = $1 AND m.left_at IS NULL))
		ORDER BY d.updated_at DESC`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Doc{}
	for rows.Next() {
		var d Doc
		if err := rows.Scan(&d.ID, &d.Title, &d.Owner, &d.ConvID, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) Rename(ctx context.Context, docID uuid.UUID, title string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE docs SET title=$2, updated_at=now() WHERE id=$1`, docID, title)
	return err
}

// AppendUpdate persists one Yjs update blob and bumps updated_at.
func (s *Store) AppendUpdate(ctx context.Context, docID uuid.UUID, data []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Yjs updates are idempotent; dedupe identical blobs so reconnect
	// storms (client re-dumps its full state per handshake) can't bloat
	// the log. 相同字节即相同 update，去重防重连风暴刷库。
	if _, err := tx.Exec(ctx, `
		INSERT INTO doc_updates (doc_id, data)
		SELECT $1, $2
		WHERE NOT EXISTS (SELECT 1 FROM doc_updates WHERE doc_id=$1 AND data=$2)`,
		docID, data); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE docs SET updated_at=now() WHERE id=$1`, docID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LoadUpdates replays the full update log for a joining client.
func (s *Store) LoadUpdates(ctx context.Context, docID uuid.UUID) ([][]byte, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT data FROM doc_updates WHERE doc_id=$1 ORDER BY seq`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
