// Package settings reads/writes workspace_settings (key → jsonb value).
// Layered policy keys live here with their defaults; admin writes go
// through the same store so every consumer sees fresh values.
//
// settings 包读写 workspace_settings。策略键与默认值集中在此。
package settings

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Policy keys / 策略键
const (
	KeyIMMessageMaxChars = "im.message_max_chars"    // int, default 2000, hard cap 5000
	KeyIMRecallTimeout   = "im.recall_timeout_sec"   // int seconds, default 300
	KeyIMReadReceipts    = "im.read_receipts"        // bool, default true
	KeyIMAtAll           = "im.at_all"               // "all" | "admin", default "all"
	KeyStorageQuota      = "storage.quota_bytes"     // int bytes, 0 = unlimited
	KeyStorageUploadMax  = "storage.upload_max_mb"   // int MB per file, default 5
	KeyMailOutbound      = "mail.outbound_enabled"   // bool, default true
	KeySecurityMinPass   = "security.min_password"   // int chars, default 12
	KeySecurityLockN     = "security.lock_threshold" // int fails, default 5
	KeySecurityLockMin   = "security.lock_minutes"   // int minutes, default 15
)

// Defaults / 默认值
const (
	DefMessageMaxChars = int64(2000)
	MaxMessageCharsCap = int64(5000)
	DefRecallTimeout   = int64(300)
	DefUploadMaxMB     = int64(5)
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) get(ctx context.Context, key string) (json.RawMessage, bool) {
	var v json.RawMessage
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM workspace_settings WHERE key=$1`, key).Scan(&v)
	return v, err == nil
}

// Int returns the setting as int64, or def when absent/invalid.
func (s *Store) Int(ctx context.Context, key string, def int64) int64 {
	raw, ok := s.get(ctx, key)
	if !ok {
		return def
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err == nil {
		return v
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return int64(f)
	}
	return def
}

func (s *Store) Bool(ctx context.Context, key string, def bool) bool {
	raw, ok := s.get(ctx, key)
	if !ok {
		return def
	}
	var v bool
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	return def
}

func (s *Store) Str(ctx context.Context, key, def string) string {
	raw, ok := s.get(ctx, key)
	if !ok {
		return def
	}
	var v string
	if json.Unmarshal(raw, &v) == nil && v != "" {
		return v
	}
	return def
}

// Set upserts a setting; updated_by tracks which admin changed it.
func (s *Store) Set(ctx context.Context, key string, value any, updatedBy uuid.UUID) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO workspace_settings (key, value, updated_by)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (key) DO UPDATE SET value=$2, updated_by=$3, updated_at=now()`,
		key, raw, updatedBy)
	return err
}

// All returns every stored setting for admin display.
func (s *Store) All(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM workspace_settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v json.RawMessage
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
