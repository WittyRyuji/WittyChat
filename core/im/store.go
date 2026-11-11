// Package im implements the messaging domain: conversations, members,
// the per-conversation op stream, and sync reads.
//
// im 包实现消息域：会话、成员、每会话 op 流与同步读取。
package im

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ConvKindDM        = "dm"
	ConvKindGroup     = "group"
	ConvKindAssistant = "assistant"

	OpMessage      = "message"
	OpEdit         = "edit"
	OpRecall       = "recall"
	OpMemberAdd    = "member_add"
	OpMemberRemove = "member_remove"
	OpRoleChange   = "role_change"
	OpRename       = "rename"
	OpAvatar       = "avatar"
	OpTransfer     = "transfer"
	OpReaction     = "reaction"

	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

var (
	ErrNotMember     = errors.New("not a conversation member")
	ErrNotFound      = errors.New("conversation not found")
	ErrNoPermission  = errors.New("no permission")
	ErrRecallExpired = errors.New("recall window expired")
	ErrBadTarget     = errors.New("bad target op")
	ErrBadEmoji      = errors.New("bad emoji")
	ErrDMExists      = errors.New("dm already exists")
	ErrSelfDM        = errors.New("cannot dm yourself")
)

// DefaultRecallWindow — used unless overridden via Store.RecallWindow
// (workspace setting im.recall_timeout_sec). 撤回时限默认 10 分钟。
const DefaultRecallWindow = 10 * time.Minute

// Querier abstracts *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool         *pgxpool.Pool
	RecallWindow time.Duration // recall timeout; set at boot, refreshable
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, RecallWindow: DefaultRecallWindow}
}

// Conversation row + my membership view.
type Conversation struct {
	ID          uuid.UUID
	Kind        string
	Title       string
	IsPublic    bool
	OwnerID     *uuid.UUID
	LastOpSeq   int64
	MyRole      string
	LastReadSeq int64
	JoinedAt    time.Time
}

// Member of a conversation.
type Member struct {
	UserID      uuid.UUID
	Role        string
	JoinedAt    time.Time
	LeftAt      *time.Time
	Name        string // display_name join
	LastReadSeq int64  // read watermark → 已读回执聚合
}

func (s *Store) conv(ctx context.Context, q Querier, id uuid.UUID) (*Conversation, error) {
	var c Conversation
	err := q.QueryRow(ctx,
		`SELECT id, kind, COALESCE(title,''), is_public, owner_id, last_op_seq
		 FROM conversations WHERE id=$1`, id).
		Scan(&c.ID, &c.Kind, &c.Title, &c.IsPublic, &c.OwnerID, &c.LastOpSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// memberRole returns my role, or ErrNotMember (also when left).
func (s *Store) memberRole(ctx context.Context, q Querier, convID, userID uuid.UUID) (string, error) {
	var role string
	err := q.QueryRow(ctx,
		`SELECT role FROM conv_members WHERE conv_id=$1 AND user_id=$2 AND left_at IS NULL`,
		convID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotMember
	}
	return role, err
}

// requireMember loads the conv and checks active membership.
func (s *Store) requireMember(ctx context.Context, q Querier, convID, userID uuid.UUID) (*Conversation, string, error) {
	c, err := s.conv(ctx, q, convID)
	if err != nil {
		return nil, "", err
	}
	role, err := s.memberRole(ctx, q, convID, userID)
	if err != nil {
		return nil, "", err
	}
	return c, role, nil
}

// dmKey is the canonical dedup key for a private chat: "min:max" user ids.
func dmKey(a, b uuid.UUID) string {
	if a.String() < b.String() {
		return a.String() + ":" + b.String()
	}
	return b.String() + ":" + a.String()
}

// CreateDM returns the existing dm or creates it (dm_key dedup).
func (s *Store) CreateDM(ctx context.Context, me, peer uuid.UUID) (*Conversation, error) {
	if me == peer {
		return nil, ErrSelfDM
	}
	key := dmKey(me, peer)

	var existing uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM conversations WHERE dm_key=$1`, key).Scan(&existing)
	if err == nil {
		// Rejoin if I had left.
		_, _ = s.pool.Exec(ctx,
			`UPDATE conv_members SET left_at=NULL WHERE conv_id=$1 AND user_id=$2`, existing, me)
		return s.conv(ctx, s.pool, existing)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	id := uuid.Must(uuid.NewV7())
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO conversations (id, kind, is_public, dm_key) VALUES ($1,'dm',false,$2)`,
			id, key); err != nil {
			return err
		}
		for _, uid := range []uuid.UUID{me, peer} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO conv_members (conv_id, user_id, role) VALUES ($1,$2,'member')
				 ON CONFLICT (conv_id, user_id) DO UPDATE SET left_at=NULL`,
				id, uid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Race: someone else created it first → fetch theirs.
		if e := s.pool.QueryRow(ctx,
			`SELECT id FROM conversations WHERE dm_key=$1`, key).Scan(&existing); e == nil {
			return s.conv(ctx, s.pool, existing)
		}
		return nil, err
	}
	return s.conv(ctx, s.pool, id)
}

// CreateGroup creates a group; creator becomes owner.
func (s *Store) CreateGroup(ctx context.Context, owner uuid.UUID, title string, isPublic bool, memberIDs []uuid.UUID) (*Conversation, error) {
	id := uuid.Must(uuid.NewV7())
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO conversations (id, kind, title, is_public, owner_id)
			 VALUES ($1,'group',$2,$3,$4)`, id, title, isPublic, owner); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO conv_members (conv_id, user_id, role) VALUES ($1,$2,'owner')`,
			id, owner); err != nil {
			return err
		}
		for _, uid := range memberIDs {
			if uid == owner {
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO conv_members (conv_id, user_id, role) VALUES ($1,$2,'member')
				 ON CONFLICT DO NOTHING`, id, uid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.conv(ctx, s.pool, id)
}

// AddMembers adds users to a group (owner/admin only). Emits member_add ops.
func (s *Store) AddMembers(ctx context.Context, actor, convID uuid.UUID, userIDs []uuid.UUID) ([]uuid.UUID, error) {
	var added []uuid.UUID
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, role, err := s.requireMember(ctx, tx, convID, actor)
		if err != nil {
			return err
		}
		if c.Kind != ConvKindGroup {
			return ErrNoPermission
		}
		if role != RoleOwner && role != RoleAdmin {
			return ErrNoPermission
		}
		for _, uid := range userIDs {
			tag, err := tx.Exec(ctx,
				`INSERT INTO conv_members (conv_id, user_id, role) VALUES ($1,$2,'member')
				 ON CONFLICT (conv_id, user_id) DO UPDATE SET left_at=NULL
				 WHERE conv_members.left_at IS NOT NULL`, convID, uid)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue // already an active member
			}
			added = append(added, uid)
			if _, err := s.appendOpTx(ctx, tx, convID, actor, OpMemberAdd,
				nil, 0, "", map[string]any{"user_id": uid.String()}); err != nil {
				return err
			}

		}
		return nil
	})
	return added, err
}

// RemoveMember removes a member (owner/admin, or self-leave). Emits member_remove op.
func (s *Store) RemoveMember(ctx context.Context, actor, convID, target uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, role, err := s.requireMember(ctx, tx, convID, actor)
		if err != nil {
			return err
		}
		selfLeave := actor == target
		if c.Kind == ConvKindDM && !selfLeave {
			return ErrNoPermission
		}
		if !selfLeave && role != RoleOwner && role != RoleAdmin {
			return ErrNoPermission
		}
		tag, err := tx.Exec(ctx,
			`UPDATE conv_members SET left_at=now() WHERE conv_id=$1 AND user_id=$2 AND left_at IS NULL`,
			convID, target)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotMember
		}
		_, err = s.appendOpTx(ctx, tx, convID, actor, OpMemberRemove,
			nil, 0, "", map[string]any{"user_id": target.String()})
		return err
	})
}

// Members lists active members (with names) of a conv the user belongs to.
func (s *Store) Members(ctx context.Context, convID, userID uuid.UUID) ([]*Member, error) {
	if _, _, err := s.requireMember(ctx, s.pool, convID, userID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.user_id, m.role, m.joined_at, u.display_name, m.last_read_op_seq
		 FROM conv_members m JOIN users u ON u.id=m.user_id
		 WHERE m.conv_id=$1 AND m.left_at IS NULL ORDER BY m.joined_at`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Role, &m.JoinedAt, &m.Name, &m.LastReadSeq); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// MemberIDs returns active member user ids (for WS fan-out).
func (s *Store) MemberIDs(ctx context.Context, convID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id FROM conv_members WHERE conv_id=$1 AND left_at IS NULL`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// IsMember is a cheap membership check (WS typing etc).
func (s *Store) IsMember(ctx context.Context, convID, userID uuid.UUID) bool {
	_, err := s.memberRole(ctx, s.pool, convID, userID)
	return err == nil
}

// MemberRole returns the caller's role in the conversation.
func (s *Store) MemberRole(ctx context.Context, convID, userID uuid.UUID) (string, error) {
	return s.memberRole(ctx, s.pool, convID, userID)
}

// convTitle helper for errors.
func (s *Store) String() string { return fmt.Sprintf("im.Store(%p)", s.pool) }
