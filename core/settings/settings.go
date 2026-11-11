// Package settings reads/writes workspace_settings (key → jsonb value).
// Layered policy keys live here with their defaults; admin writes go
// through the same store so every consumer sees fresh values.
//
// settings 包读写 workspace_settings。策略键与默认值集中在此。
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"wittychat/core/objects"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	KeyStorageLargeMax   = "storage.large_max_mb"    // int MB per S3 multipart file, default 2048
	KeyStorageColdDays   = "storage.cold_days"       // int days before cold→s3, 0 = off, default 90
	KeyMailOutbound      = "mail.outbound_enabled"   // bool, default true
	KeyMailDKIMKey       = "mail.dkim_key"           // string PEM, RSA private key
	KeyMailDKIMSelector  = "mail.dkim_selector"      // string, default "s1"
	KeySecurityMinPass   = "security.min_password"   // int chars, default 15 (NIST single-factor)
	KeySecurityLockN     = "security.lock_threshold" // int fails, default 5
	KeySecurityLockMin   = "security.lock_minutes"   // int minutes, default 15
	KeyDocsLogMaxMB      = "docs.log_max_mb"         // int MB of Yjs log per doc, default 100
	KeyAuditRetention    = "audit.retention_days"    // int days to keep versions + doc archive, default 180
)

// Defaults / 默认值
const (
	DefMessageMaxChars = int64(2000)
	MaxMessageCharsCap = int64(5000)
	DefRecallTimeout   = int64(300)
	DefUploadMaxMB     = int64(5)
	DefLargeMaxMB      = int64(2048)
	DefColdDays        = int64(90)
	DefMinPassword     = int64(15)
	DefLockThreshold   = int64(5)
	DefLockMinutes     = int64(15)
	DefDocsLogMaxMB    = int64(100)
	DefAuditRetention  = int64(180)
)

var ErrInvalidSetting = errors.New("invalid setting")

// IntBounds bounds numeric policy keys. Admin writes outside the range
// are rejected — a 0 or negative value must not silently disable a
// safeguard (e.g. retention=0 would purge ALL version history at the
// next GC pass). Unlisted keys are unrestricted (strings, bools,
// free-form values like logo ids). / 数值策略键取值范围。超范围的后台
// 写入被拒绝——0 或负值不能悄悄关掉防护（retention=0 会在下一次 GC
// 清空全部历史版本）。未列出的键不受约束（字符串、布尔、logo id 等）。
var IntBounds = map[string][2]int64{
	KeyIMMessageMaxChars: {1, MaxMessageCharsCap},
	KeyIMRecallTimeout:   {10, 3600},
	KeySecurityMinPass:   {8, 256},
	KeySecurityLockN:     {1, 100},
	KeySecurityLockMin:   {1, 1440},
	KeyDocsLogMaxMB:      {0, 1024}, // 0 = unlimited / 0=不限
	KeyAuditRetention:    {1, 36500},
	KeyStorageQuota:      {0, 1 << 50},
	KeyStorageUploadMax:  {1, 2048},
	KeyStorageLargeMax:   {1, 102400},
	KeyStorageColdDays:   {0, 3650}, // 0 = cold migration off / 0=关闭冷迁移
}

// ValidateInt checks a numeric policy value against IntBounds.
// 按 IntBounds 校验数值型策略值。
func ValidateInt(key string, v int64) error {
	b, ok := IntBounds[key]
	if !ok {
		return nil
	}
	if v < b[0] || v > b[1] {
		return fmt.Errorf("%w: setting %s out of range [%d, %d] / 设置 %s 超出范围 [%d, %d]", ErrInvalidSetting, key, b[0], b[1], key, b[0], b[1])
	}
	return nil
}

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

// Int returns the setting as int64, or def when absent/invalid. Values
// outside IntBounds also fall back to def — a poisoned value written
// before bounds existed (e.g. retention=0, which would purge ALL history
// at the next GC) can't take effect through any reader.
// 读数值设置；缺失/非法时回落默认。超出 IntBounds 的存量值同样回落——
// 边界生效前写入的毒值（如 retention=0，下次 GC 会清空全部历史）无法
// 经由任何读取路径生效。
func (s *Store) Int(ctx context.Context, key string, def int64) int64 {
	raw, ok := s.get(ctx, key)
	if !ok {
		return def
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err == nil {
	} else {
		var f float64
		if json.Unmarshal(raw, &f) != nil {
			return def
		}
		v = int64(f)
	}
	if ValidateInt(key, v) != nil {
		return def
	}
	return v
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
	return s.SetMany(ctx, map[string]any{key: value}, updatedBy)
}

// SetMany validates the whole patch before writing and commits it atomically.
// Unchanged logos are not rebound: legacy SVGs and another admin's logo must
// not prevent an unrelated policy change.
// 整批校验并原子提交；未变更的 Logo 不重新绑定，兼容旧 SVG 和其他管理员的 Logo。
func (s *Store) SetMany(ctx context.Context, values map[string]any, updatedBy uuid.UUID) error {
	rawValues := make(map[string]json.RawMessage, len(values))
	keys := make([]string, 0, len(values))
	for key, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if bounds, ok := IntBounds[key]; ok {
			var n float64
			if string(raw) == "null" || json.Unmarshal(raw, &n) != nil ||
				math.Trunc(n) != n || n < float64(bounds[0]) || n > float64(bounds[1]) {
				return fmt.Errorf("%w: %s must be an integer in [%d, %d]", ErrInvalidSetting, key, bounds[0], bounds[1])
			}
		}
		rawValues[key] = raw
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		const logoKey = "workspace.logo_object_id"
		if raw, ok := rawValues[logoKey]; ok {
			var id string
			if string(raw) == "null" || json.Unmarshal(raw, &id) != nil {
				return objects.ErrNotImage
			}
			var oid uuid.UUID
			if id != "" {
				var err error
				oid, err = uuid.Parse(id)
				if err != nil {
					return objects.ErrNotFound
				}
				id = oid.String()
			}
			// Serialize even when the setting row does not yet exist.
			// 设置行不存在时也串行化 Logo 写入，避免读取旧引用后覆盖新 Logo。
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, logoKey); err != nil {
				return err
			}
			var current string
			err := tx.QueryRow(ctx, `SELECT COALESCE(value #>> '{}','') FROM workspace_settings WHERE key=$1`, logoKey).Scan(&current)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if currentID, err := uuid.Parse(current); err == nil {
				current = currentID.String()
			}
			if current == id {
				delete(rawValues, logoKey)
			} else if id != "" {
				if err := objects.RequireReadyImage(ctx, tx, oid, updatedBy); err != nil {
					return err
				}
			}
			if _, changed := rawValues[logoKey]; changed {
				raw, err = json.Marshal(id)
				if err != nil {
					return err
				}
				rawValues[logoKey] = raw
			}
		}
		for _, key := range keys {
			raw, ok := rawValues[key]
			if !ok {
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO workspace_settings (key, value, updated_by)
				 VALUES ($1,$2,$3)
				 ON CONFLICT (key) DO UPDATE SET value=$2, updated_by=$3, updated_at=now()`,
				key, raw, updatedBy); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------- per-user prefs (user_prefs) / 用户级偏好 ----------

// UAll returns every pref of one user.
func (s *Store) UAll(ctx context.Context, userID uuid.UUID) (map[string]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM user_prefs WHERE user_id=$1 ORDER BY key`, userID)
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

// USet upserts one pref key for a user.
func (s *Store) USet(ctx context.Context, userID uuid.UUID, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO user_prefs (user_id, key, value)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (user_id, key) DO UPDATE SET value=$3, updated_at=now()`,
		userID, key, raw)
	return err
}

// UDelete removes one pref key (no-op when absent).
func (s *Store) UDelete(ctx context.Context, userID uuid.UUID, key string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM user_prefs WHERE user_id=$1 AND key=$2`, userID, key)
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
