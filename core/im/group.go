package im

// Group management beyond membership: rename, transfer, public discovery,
// roles, message search. All state changes ride the op stream so clients
// see them via the normal sync path.
//
// 群管理：改名/转让/公开群发现/角色/消息搜索。所有变更走 op 流。

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RenameConv renames a group (owner/admin). Emits a 'rename' op.
func (s *Store) RenameConv(ctx context.Context, actor, convID uuid.UUID, title string) (int64, error) {
	var seq int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, role, err := s.requireMember(ctx, tx, convID, actor)
		if err != nil {
			return err
		}
		if c.Kind != ConvKindGroup || (role != RoleOwner && role != RoleAdmin) {
			return ErrNoPermission
		}
		if _, err := tx.Exec(ctx,
			`UPDATE conversations SET title=$2 WHERE id=$1`, convID, title); err != nil {
			return err
		}
		seq, err = s.appendOpTx(ctx, tx, convID, actor, OpRename,
			nil, 0, "", map[string]any{"title": title})
		return err
	})
	return seq, err
}

// TransferOwner hands ownership to another member (owner only).
// Old owner keeps 'admin' role; emits a 'transfer' op.
func (s *Store) TransferOwner(ctx context.Context, actor, convID, target uuid.UUID) (int64, error) {
	var seq int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, role, err := s.requireMember(ctx, tx, convID, actor)
		if err != nil {
			return err
		}
		if c.Kind != ConvKindGroup || role != RoleOwner {
			return ErrNoPermission
		}
		if _, err := s.memberRole(ctx, tx, convID, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE conversations SET owner_id=$2 WHERE id=$1`, convID, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE conv_members SET role='admin' WHERE conv_id=$1 AND user_id=$2`,
			convID, actor); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE conv_members SET role='owner' WHERE conv_id=$1 AND user_id=$2`,
			convID, target); err != nil {
			return err
		}
		seq, err = s.appendOpTx(ctx, tx, convID, actor, OpTransfer,
			nil, 0, "", map[string]any{"from": actor.String(), "to": target.String()})
		return err
	})
	return seq, err
}

// SetMemberRole promotes/demotes a member to admin|member (owner only).
func (s *Store) SetMemberRole(ctx context.Context, actor, convID, target uuid.UUID, role string) (int64, error) {
	if role != RoleAdmin && role != RoleMember {
		return 0, ErrNoPermission
	}
	var seq int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, my, err := s.requireMember(ctx, tx, convID, actor)
		if err != nil {
			return err
		}
		if c.Kind != ConvKindGroup || my != RoleOwner {
			return ErrNoPermission
		}
		if target == actor {
			return ErrNoPermission // transfer ownership instead
		}
		if _, err := s.memberRole(ctx, tx, convID, target); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE conv_members SET role=$3 WHERE conv_id=$1 AND user_id=$2`,
			convID, target, role); err != nil {
			return err
		}
		seq, err = s.appendOpTx(ctx, tx, convID, actor, OpRoleChange,
			nil, 0, "", map[string]any{"user_id": target.String(), "role": role})
		return err
	})
	return seq, err
}

// PublicGroup is a searchable public conversation.
type PublicGroup struct {
	ID          uuid.UUID
	Title       string
	MemberCount int
	Joined      bool
}

// SearchPublicGroups finds public groups by title substring (empty q → all).
func (s *Store) SearchPublicGroups(ctx context.Context, userID uuid.UUID, q string, limit int) ([]*PublicGroup, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, COALESCE(c.title,''),
		        (SELECT count(*) FROM conv_members m WHERE m.conv_id=c.id AND m.left_at IS NULL),
		        EXISTS(SELECT 1 FROM conv_members m
		               WHERE m.conv_id=c.id AND m.user_id=$2 AND m.left_at IS NULL)
		 FROM conversations c
		 WHERE c.kind='group' AND c.is_public AND ($3='' OR c.title ILIKE '%'||$3||'%')
		 ORDER BY c.created_at DESC LIMIT $1`, limit, userID, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PublicGroup
	for rows.Next() {
		var g PublicGroup
		if err := rows.Scan(&g.ID, &g.Title, &g.MemberCount, &g.Joined); err != nil {
			return nil, err
		}
		out = append(out, &g)
	}
	return out, rows.Err()
}

// JoinPublicGroup adds the caller to a public group (no admin needed).
func (s *Store) JoinPublicGroup(ctx context.Context, convID, user uuid.UUID) (int64, error) {
	var seq int64 = -1
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var kind string
		var pub bool
		err := tx.QueryRow(ctx,
			`SELECT kind, is_public FROM conversations WHERE id=$1`, convID).Scan(&kind, &pub)
		if err != nil {
			return ErrNotFound
		}
		if kind != ConvKindGroup || !pub {
			return ErrNoPermission
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO conv_members (conv_id, user_id, role) VALUES ($1,$2,'member')
			 ON CONFLICT (conv_id, user_id) DO UPDATE SET left_at=NULL
			 WHERE conv_members.left_at IS NOT NULL`, convID, user)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already a member — no op
		}
		seq, err = s.appendOpTx(ctx, tx, convID, user, OpMemberAdd,
			nil, 0, "", map[string]any{"user_id": user.String(), "via": "public_join"})
		return err
	})
	return seq, err
}

// MessageHit is one search result.
type MessageHit struct {
	ConvID    uuid.UUID
	OpSeq     int64
	Body      string
	ActorName string
	ConvTitle string
	CreatedAt string
}

// SearchMessages searches message bodies across the caller's conversations.
func (s *Store) SearchMessages(ctx context.Context, userID uuid.UUID, q string, limit int) ([]*MessageHit, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT o.conv_id, o.op_seq, COALESCE(e.payload->>'markdown', o.body),
		        COALESCE(u.display_name,''),
		        COALESCE(c.title,''), o.created_at::text
		 FROM conv_ops o
		 JOIN conv_members m ON m.conv_id=o.conv_id AND m.user_id=$1 AND m.left_at IS NULL
		 JOIN conversations c ON c.id=o.conv_id
		 LEFT JOIN users u ON u.id=o.actor_id
		 LEFT JOIN LATERAL (
		   SELECT payload FROM conv_ops e
		   WHERE e.conv_id=o.conv_id AND e.kind='edit' AND e.target_op_seq=o.op_seq
		   ORDER BY e.op_seq DESC LIMIT 1
		 ) e ON true
		 WHERE o.kind='message'
		   AND COALESCE(e.payload->>'markdown', o.body) ILIKE '%'||$2||'%'
		   AND NOT EXISTS (
		     SELECT 1 FROM conv_ops r
		     WHERE r.conv_id=o.conv_id AND r.kind='recall' AND r.target_op_seq=o.op_seq)
		 ORDER BY o.created_at DESC LIMIT $3`, userID, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MessageHit
	for rows.Next() {
		var h MessageHit
		if err := rows.Scan(&h.ConvID, &h.OpSeq, &h.Body, &h.ActorName, &h.ConvTitle, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &h)
	}
	return out, rows.Err()
}
