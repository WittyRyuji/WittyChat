package im

// Sync reads: conversation directory + per-conversation op pages.
// Clients compare the directory watermarks, then fetch only deltas —
// a client absent for a weekend never touches unchanged conversations.
//
// 同步读取：会话目录 + 每会话 op 分页。
// 客户端先比对目录水印，只补变动会话的差额——
// 周末没登录的客户端不会碰没变化的会话。

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DirectoryEntry — one row of the client's conversation list.
type DirectoryEntry struct {
	ConvID      uuid.UUID
	Kind        string
	Title       string
	IsPublic    bool
	LastOpSeq   int64
	LastReadSeq int64
	Unread      int64
	Preview     string
	PreviewAt   time.Time
	PeerUserID  *uuid.UUID // dm: the other party
	PeerName    string
	MemberCount int
}

// Directory returns all my conversations with watermarks + unread counts.
// One query; cheap — conv_members index on (user_id) where left_at IS NULL.
func (s *Store) Directory(ctx context.Context, userID uuid.UUID) ([]*DirectoryEntry, error) {
	rows, err := s.pool.Query(ctx, `
SELECT c.id, c.kind, COALESCE(c.title,''), c.is_public, c.last_op_seq,
       m.last_read_op_seq,
       (SELECT count(*) FROM conv_ops o WHERE o.conv_id=c.id
          AND o.kind='message' AND o.op_seq > m.last_read_op_seq
          AND o.actor_id IS DISTINCT FROM m.user_id) AS unread,
       COALESCE((SELECT o.body FROM conv_ops o WHERE o.conv_id=c.id AND o.kind='message'
          ORDER BY o.op_seq DESC LIMIT 1), '') AS preview,
       (SELECT o.created_at FROM conv_ops o WHERE o.conv_id=c.id AND o.kind='message'
          ORDER BY o.op_seq DESC LIMIT 1) AS preview_at,
       (SELECT count(*) FROM conv_members cm WHERE cm.conv_id=c.id AND cm.left_at IS NULL),
       CASE WHEN c.kind='dm' THEN
         (SELECT cm.user_id FROM conv_members cm WHERE cm.conv_id=c.id
            AND cm.user_id <> $1 AND cm.left_at IS NULL LIMIT 1)
       END AS peer_id
FROM conv_members m
JOIN conversations c ON c.id = m.conv_id
WHERE m.user_id=$1 AND m.left_at IS NULL
ORDER BY c.last_op_seq DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DirectoryEntry
	peerIDs := map[uuid.UUID]*DirectoryEntry{}
	for rows.Next() {
		e := &DirectoryEntry{}
		var previewAt *time.Time
		var peer *uuid.UUID
		if err := rows.Scan(&e.ConvID, &e.Kind, &e.Title, &e.IsPublic, &e.LastOpSeq,
			&e.LastReadSeq, &e.Unread, &e.Preview, &previewAt, &e.MemberCount, &peer); err != nil {
			return nil, err
		}
		if previewAt != nil {
			e.PreviewAt = *previewAt
		}
		e.PeerUserID = peer
		out = append(out, e)
		if peer != nil {
			peerIDs[*peer] = e
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Resolve dm peer names in one query.
	if len(peerIDs) > 0 {
		ids := make([]uuid.UUID, 0, len(peerIDs))
		for id := range peerIDs {
			ids = append(ids, id)
		}
		nrows, err := s.pool.Query(ctx,
			`SELECT id, display_name FROM users WHERE id = ANY($1)`, ids)
		if err == nil {
			defer nrows.Close()
			for nrows.Next() {
				var id uuid.UUID
				var name string
				if err := nrows.Scan(&id, &name); err == nil {
					if e, ok := peerIDs[id]; ok {
						e.PeerName = name
					}
				}
			}
		}
	}
	return out, nil
}

// FetchOpsAfter returns ops with seq > after, ascending, up to limit.
// has_more=true means another page exists.
// Membership required. Effective view applied (edits merged, recalls redacted).
func (s *Store) FetchOpsAfter(ctx context.Context, userID, convID uuid.UUID, after int64, limit int) ([]*Op, bool, int64, error) {
	c, err := s.conv(ctx, s.pool, convID)
	if err != nil {
		return nil, false, 0, err
	}
	if !s.IsMember(ctx, convID, userID) {
		return nil, false, 0, ErrNotMember
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
SELECT o.op_seq, o.op_id, o.kind, o.actor_id, u.display_name, o.target_op_seq,
       COALESCE(o.body,''), o.payload, o.client_msg_id, o.created_at
FROM conv_ops o LEFT JOIN users u ON u.id = o.actor_id
WHERE o.conv_id=$1 AND o.op_seq > $2
ORDER BY o.op_seq ASC LIMIT $3`, convID, after, limit+1)
	if err != nil {
		return nil, false, 0, err
	}
	defer rows.Close()

	ops, err := scanOps(rows)
	if err != nil {
		return nil, false, 0, err
	}
	hasMore := len(ops) > limit
	if hasMore {
		ops = ops[:limit]
	}
	if err := s.applyEffectiveView(ctx, convID, ops); err != nil {
		return nil, false, 0, err
	}
	return ops, hasMore, c.LastOpSeq, nil
}

// FetchOpsBefore returns ops with seq < before, descending → reversed ascending.
// Used to backfill history when opening a conversation.
func (s *Store) FetchOpsBefore(ctx context.Context, userID, convID uuid.UUID, before int64, limit int) ([]*Op, bool, error) {
	if _, err := s.conv(ctx, s.pool, convID); err != nil {
		return nil, false, err
	}
	if !s.IsMember(ctx, convID, userID) {
		return nil, false, ErrNotMember
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows pgx.Rows
	var err error
	if before <= 0 {
		rows, err = s.pool.Query(ctx, `
SELECT o.op_seq, o.op_id, o.kind, o.actor_id, u.display_name, o.target_op_seq,
       COALESCE(o.body,''), o.payload, o.client_msg_id, o.created_at
FROM conv_ops o LEFT JOIN users u ON u.id = o.actor_id
WHERE o.conv_id=$1 ORDER BY o.op_seq DESC LIMIT $2`, convID, limit+1)
	} else {
		rows, err = s.pool.Query(ctx, `
SELECT o.op_seq, o.op_id, o.kind, o.actor_id, u.display_name, o.target_op_seq,
       COALESCE(o.body,''), o.payload, o.client_msg_id, o.created_at
FROM conv_ops o LEFT JOIN users u ON u.id = o.actor_id
WHERE o.conv_id=$1 AND o.op_seq < $2 ORDER BY o.op_seq DESC LIMIT $3`, convID, before, limit+1)
	}
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	ops, err := scanOps(rows)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(ops) > limit
	if hasMore {
		ops = ops[:limit]
	}
	// Came back DESC → flip to ASC for the client.
	sort.Slice(ops, func(i, j int) bool { return ops[i].Seq < ops[j].Seq })
	if err := s.applyEffectiveView(ctx, convID, ops); err != nil {
		return nil, false, err
	}
	return ops, hasMore, nil
}

func scanOps(rows pgx.Rows) ([]*Op, error) {
	var out []*Op
	for rows.Next() {
		var o Op
		var target *int64
		var clientMsg *uuid.UUID
		var actorName *string
		if err := rows.Scan(&o.Seq, &o.ID, &o.Kind, &o.ActorID, &actorName,
			&target, &o.Body, &o.Payload, &clientMsg, &o.CreatedAt); err != nil {
			return nil, err
		}
		if target != nil {
			o.TargetSeq = *target
		}
		o.ClientMsg = clientMsg
		if actorName != nil {
			o.ActorName = *actorName
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// applyEffectiveView merges later edits into returned messages and redacts
// recalled ones. Original rows stay in the table for admin audit.
// 有效视图：把之后的编辑合并进消息、撤回的消息脱敏。原始行留库供审计。
func (s *Store) applyEffectiveView(ctx context.Context, convID uuid.UUID, ops []*Op) error {
	msgSeqs := make([]int64, 0, len(ops))
	bySeq := map[int64]*Op{}
	for _, o := range ops {
		if o.Kind == OpMessage {
			msgSeqs = append(msgSeqs, o.Seq)
			bySeq[o.Seq] = o
			continue
		}
	}
	if len(msgSeqs) == 0 {
		return nil
	}

	// Latest edit per target among all edit ops in this conv.
	erows, err := s.pool.Query(ctx, `
SELECT DISTINCT ON (target_op_seq) target_op_seq, payload, created_at
FROM conv_ops
WHERE conv_id=$1 AND kind='edit' AND target_op_seq = ANY($2)
ORDER BY target_op_seq, op_seq DESC`, convID, msgSeqs)
	if err != nil {
		return err
	}
	defer erows.Close()
	for erows.Next() {
		var tgt int64
		var payload map[string]any
		var at time.Time
		if err := erows.Scan(&tgt, &payload, &at); err != nil {
			return err
		}
		if o, ok := bySeq[tgt]; ok {
			if md, ok := payload["markdown"].(string); ok {
				o.Body = md
				o.Payload["markdown"] = md
			}
			o.Edited = true
		}
	}
	erows.Close()

	// Any recall targeting returned messages → redact.
	rrows, err := s.pool.Query(ctx, `
SELECT target_op_seq FROM conv_ops
WHERE conv_id=$1 AND kind='recall' AND target_op_seq = ANY($2)`, convID, msgSeqs)
	if err != nil {
		return err
	}
	defer rrows.Close()
	for rrows.Next() {
		var tgt int64
		if err := rrows.Scan(&tgt); err != nil {
			return err
		}
		if o, ok := bySeq[tgt]; ok {
			o.Recalled = true
			o.Body = ""
			o.Payload = map[string]any{"recalled": true}
		}
	}
	return rrows.Err()
}
