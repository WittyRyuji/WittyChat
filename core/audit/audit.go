// Package audit writes audit_events rows. All sensitive actions (admin ops,
// auth events, assistant approvals) must emit an event — including admin reads
// of retained private data.
//
// audit 包写 audit_events。所有敏感操作（管理操作、认证事件、助理审批）
// 必须落事件——包括管理员查看留存的私人数据。
package audit

import (
	"context"
	"encoding/json"
	"net"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Actor kinds / 行为者类型
const (
	ActorUser      = "user"
	ActorAdmin     = "admin"
	ActorAssistant = "assistant"
	ActorSystem    = "system"
	ActorHook      = "hook"
)

type Emitter struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Emitter { return &Emitter{pool: pool} }

// Log inserts one audit event. detail is marshalled to jsonb.
func (e *Emitter) Log(ctx context.Context, actorID *uuid.UUID, actorKind, action, target string, detail any, ip net.IP) {
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		raw = []byte(`{"marshal_error":true}`)
	}
	_, _ = e.pool.Exec(ctx,
		`INSERT INTO audit_events (actor_id, actor_kind, action, target, detail, ip)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		actorID, actorKind, action, target, raw, ip)
}
