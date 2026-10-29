package im

// The op stream: send/edit/recall/read + sync fetches.
// Ordering is guaranteed by last_op_seq incremented inside the same tx.
// Idempotent sends via client_msg_id unique index.
//
// op 流：发送/编辑/撤回/已读 + 同步拉取。
// 同事务内 last_op_seq 自增保证有序；client_msg_id 唯一索引保证幂等。

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Op is one entry of a conversation's op stream.
type Op struct {
	Seq       int64
	ID        uuid.UUID
	ConvID    uuid.UUID
	Kind      string
	ActorID   *uuid.UUID
	ActorName string
	TargetSeq int64
	Body      string
	Payload   map[string]any
	ClientMsg *uuid.UUID
	Edited    bool // effective view: a later edit op targets this message
	Recalled  bool // effective view: a recall op targets this message
	CreatedAt time.Time
}

// appendOpTx increments last_op_seq and inserts the op in the same tx.
func (s *Store) appendOpTx(ctx context.Context, tx pgx.Tx, convID, actor uuid.UUID,
	kind string, clientMsgID *uuid.UUID, targetSeq int64, body string, payload map[string]any,
) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx,
		`UPDATE conversations SET last_op_seq = last_op_seq + 1 WHERE id=$1 RETURNING last_op_seq`,
		convID).Scan(&seq); err != nil {
		return 0, err
	}
	var tgt *int64
	if targetSeq > 0 {
		tgt = &targetSeq
	}
	if payload == nil {
		payload = map[string]any{}
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO conv_ops (conv_id, op_seq, op_id, kind, actor_id, client_msg_id, target_op_seq, body, payload)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		convID, seq, uuid.Must(uuid.NewV7()), kind, actor, clientMsgID, tgt, nilIfEmpty(body), payload)
	return seq, err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SendMessage appends a message op; idempotent on client_msg_id.
// Returns (op, alreadyExisted, error). Sets sender read watermark to the new seq.
func (s *Store) SendMessage(ctx context.Context, sender, convID uuid.UUID,
	clientMsgID uuid.UUID, markdown string, quoteOpSeq int64, mentions []uuid.UUID,
) (*Op, bool, error) {
	if len([]rune(markdown)) > MaxMessageLen {
		return nil, false, ErrMessageTooLong
	}
	payload := map[string]any{"markdown": markdown}
	if quoteOpSeq > 0 {
		payload["quote_op_seq"] = quoteOpSeq
	}
	if len(mentions) > 0 {
		ids := make([]string, len(mentions))
		for i, m := range mentions {
			ids[i] = m.String()
		}
		payload["mentions"] = ids
	}

	var op *Op
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, _, err := s.requireMember(ctx, tx, convID, sender); err != nil {
			return err
		}
		seq, err := s.appendOpTx(ctx, tx, convID, sender, OpMessage, &clientMsgID, 0, markdown, payload)
		if err != nil {
			return err
		}
		// Sender has obviously read their own message.
		if _, err := tx.Exec(ctx,
			`UPDATE conv_members SET last_read_op_seq=$3 WHERE conv_id=$1 AND user_id=$2`,
			convID, sender, seq); err != nil {
			return err
		}
		op = &Op{
			Seq: seq, ConvID: convID, Kind: OpMessage, ActorID: &sender,
			Body: markdown, Payload: payload, ClientMsg: &clientMsgID, CreatedAt: time.Now(),
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Idempotent retry: return the original op.
			existing, e := s.opByClientMsgID(ctx, clientMsgID)
			if e == nil {
				return existing, true, nil
			}
		}
		return nil, false, err
	}
	return op, false, nil
}

const MaxMessageLen = 5000 // hard ceiling; workspace default 2000 enforced at API layer

var ErrMessageTooLong = errors.New("message exceeds max length")

// EditMessage appends an edit op targeting a message op (author only).
func (s *Store) EditMessage(ctx context.Context, actor, convID uuid.UUID, targetSeq int64, markdown string) (int64, error) {
	if len([]rune(markdown)) > MaxMessageLen {
		return 0, ErrMessageTooLong
	}
	var seq int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, _, err := s.requireMember(ctx, tx, convID, actor); err != nil {
			return err
		}
		owner, kind, err := s.opOwner(ctx, tx, convID, targetSeq)
		if err != nil {
			return err
		}
		if kind != OpMessage || owner == nil || *owner != actor {
			return ErrNoPermission
		}
		seq, err = s.appendOpTx(ctx, tx, convID, actor, OpEdit,
			nil, targetSeq, "", map[string]any{"markdown": markdown})
		return err
	})
	return seq, err
}

// RecallMessage appends a recall op (author only, within RecallWindow).
func (s *Store) RecallMessage(ctx context.Context, actor, convID uuid.UUID, targetSeq int64) (int64, error) {
	var seq int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, _, err := s.requireMember(ctx, tx, convID, actor); err != nil {
			return err
		}
		owner, kind, created, err := s.opOwnerCreated(ctx, tx, convID, targetSeq)
		if err != nil {
			return err
		}
		if kind != OpMessage || owner == nil || *owner != actor {
			return ErrNoPermission
		}
		if time.Since(created) > RecallWindow {
			return ErrRecallExpired
		}
		seq, err = s.appendOpTx(ctx, tx, convID, actor, OpRecall, nil, targetSeq, "", nil)
		return err
	})
	return seq, err
}

// MarkRead advances my read watermark (monotonic, coalescing).
func (s *Store) MarkRead(ctx context.Context, convID, userID uuid.UUID, seq int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conv_members SET last_read_op_seq=$3
		 WHERE conv_id=$1 AND user_id=$2 AND left_at IS NULL AND last_read_op_seq < $3`,
		convID, userID, seq)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Either not a member or watermark already ahead — distinguish.
		if _, err := s.memberRole(ctx, s.pool, convID, userID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) opOwner(ctx context.Context, q Querier, convID uuid.UUID, seq int64) (*uuid.UUID, string, error) {
	var owner *uuid.UUID
	var kind string
	err := q.QueryRow(ctx,
		`SELECT actor_id, kind FROM conv_ops WHERE conv_id=$1 AND op_seq=$2`, convID, seq).
		Scan(&owner, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrBadTarget
	}
	return owner, kind, err
}

func (s *Store) opOwnerCreated(ctx context.Context, q Querier, convID uuid.UUID, seq int64) (*uuid.UUID, string, time.Time, error) {
	var owner *uuid.UUID
	var kind string
	var created time.Time
	err := q.QueryRow(ctx,
		`SELECT actor_id, kind, created_at FROM conv_ops WHERE conv_id=$1 AND op_seq=$2`, convID, seq).
		Scan(&owner, &kind, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", time.Time{}, ErrBadTarget
	}
	return owner, kind, created, err
}

func (s *Store) opByClientMsgID(ctx context.Context, clientMsgID uuid.UUID) (*Op, error) {
	var o Op
	err := s.pool.QueryRow(ctx,
		`SELECT op_seq, conv_id, kind, actor_id, body, payload, created_at
		 FROM conv_ops WHERE client_msg_id=$1`, clientMsgID).
		Scan(&o.Seq, &o.ConvID, &o.Kind, &o.ActorID, &o.Body, &o.Payload, &o.CreatedAt)
	return &o, err
}

func isUniqueViolation(err error) bool {
	// pgx wraps pgconn.PgError; check SQLSTATE 23505.
	type sqlState interface {
		SQLState() string
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if st, ok := e.(sqlState); ok && st.SQLState() == "23505" {
			return true
		}
	}
	return false
}
