// Package agents is the assistant config store. P0 freezes the surface:
// CRUD over runtime/adapter/model/instructions/scopes/session_level/
// auto_send — the runtime itself stays disabled until the ACP bridge lands.
//
// 助理配置存储。P0 冻结配置面，运行时保持禁用，待 ACP 桥接落地。
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("assistant not found")
	ErrRunning  = errors.New("runtime disabled in P0 / P0 阶段运行时未启用")
)

type Assistant struct {
	ID           uuid.UUID       `json:"id"`
	OwnerID      uuid.UUID       `json:"owner_id"`
	Name         string          `json:"name"`
	Runtime      string          `json:"runtime"` // acp | api
	Adapter      string          `json:"adapter"` // codex/qwen/claude/openai-compat…
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Scopes       json.RawMessage `json:"scopes"`        // capability scopes
	SessionLevel int             `json:"session_level"` // 1-4
	AutoSend     bool            `json:"auto_send"`     // 代发免确认
	Status       string          `json:"status"`        // disabled always in P0
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const cols = `id,owner_id,name,runtime,COALESCE(adapter,''),COALESCE(model,''),
	COALESCE(instructions,''),scopes,session_level,auto_send,status,created_at,updated_at`

func (s *Store) List(ctx context.Context, owner uuid.UUID) ([]Assistant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+cols+` FROM assistants WHERE owner_id=$1 ORDER BY created_at`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Assistant
	for rows.Next() {
		var a Assistant
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Name, &a.Runtime, &a.Adapter,
			&a.Model, &a.Instructions, &a.Scopes, &a.SessionLevel, &a.AutoSend,
			&a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type Upsert struct {
	Name         string          `json:"name"`
	Runtime      string          `json:"runtime"`
	Adapter      string          `json:"adapter"`
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Scopes       json.RawMessage `json:"scopes"`
	SessionLevel int             `json:"session_level"`
	AutoSend     bool            `json:"auto_send"`
}

func validRuntime(r string) bool { return r == "acp" || r == "api" }

func (s *Store) Create(ctx context.Context, owner uuid.UUID, u Upsert) (*Assistant, error) {
	if u.Name == "" || !validRuntime(u.Runtime) {
		return nil, errors.New("name/runtime required / 名称和运行时必填")
	}
	if len(u.Scopes) == 0 {
		u.Scopes = json.RawMessage("[]")
	}
	if u.SessionLevel < 1 || u.SessionLevel > 4 {
		u.SessionLevel = 2
	}
	a := &Assistant{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO assistants (id,owner_id,name,runtime,adapter,model,instructions,scopes,session_level,auto_send)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+cols,
		uuid.Must(uuid.NewV7()), owner, u.Name, u.Runtime, u.Adapter, u.Model,
		u.Instructions, u.Scopes, u.SessionLevel, u.AutoSend).
		Scan(&a.ID, &a.OwnerID, &a.Name, &a.Runtime, &a.Adapter, &a.Model,
			&a.Instructions, &a.Scopes, &a.SessionLevel, &a.AutoSend,
			&a.Status, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func (s *Store) Update(ctx context.Context, owner, id uuid.UUID, u Upsert) (*Assistant, error) {
	if u.Name == "" || !validRuntime(u.Runtime) {
		return nil, errors.New("name/runtime required / 名称和运行时必填")
	}
	if len(u.Scopes) == 0 {
		u.Scopes = json.RawMessage("[]")
	}
	a := &Assistant{}
	err := s.pool.QueryRow(ctx,
		`UPDATE assistants SET name=$3,runtime=$4,adapter=$5,model=$6,instructions=$7,
		 scopes=$8,session_level=$9,auto_send=$10,updated_at=now()
		 WHERE id=$1 AND owner_id=$2 RETURNING `+cols,
		id, owner, u.Name, u.Runtime, u.Adapter, u.Model, u.Instructions,
		u.Scopes, u.SessionLevel, u.AutoSend).
		Scan(&a.ID, &a.OwnerID, &a.Name, &a.Runtime, &a.Adapter, &a.Model,
			&a.Instructions, &a.Scopes, &a.SessionLevel, &a.AutoSend,
			&a.Status, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

func (s *Store) Delete(ctx context.Context, owner, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM assistants WHERE id=$1 AND owner_id=$2`, id, owner)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
