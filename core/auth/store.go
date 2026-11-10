// Package auth implements the identity domain: users, credentials, sessions,
// activation links, TOTP, login throttling.
//
// auth 包实现身份域：用户、凭证、会话、激活链接、二次验证、登录限速。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// User statuses / 用户状态
const (
	StatusPending    = "pending"
	StatusActive     = "active"
	StatusSuspended  = "suspended"
	StatusOffboarded = "offboarded"
)

var (
	ErrEmailTaken    = errors.New("email already registered")
	ErrUserNotFound  = errors.New("user not found")
	ErrUserNotActive = errors.New("user is not active")
)

type User struct {
	ID             uuid.UUID
	Email          string
	DisplayName    string
	Status         string
	IsAdmin        bool
	TOTPEnabled    bool
	AvatarObjectID *uuid.UUID
	CreatedAt      time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const userCols = `id, email, display_name, status, is_admin,
	COALESCE((SELECT totp_enabled FROM credentials c WHERE c.user_id = users.id), false),
	avatar_object_id, created_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Status, &u.IsAdmin, &u.TOTPEnabled, &u.AvatarObjectID, &u.CreatedAt)
	return &u, err
}

// CreateUser inserts a pending user + empty credential row; returns ErrEmailTaken on conflict.
// CreateUser 创建 pending 用户与空凭证行；邮箱冲突返回 ErrEmailTaken。
func (s *Store) CreateUser(ctx context.Context, email, displayName string, isAdmin bool) (*User, error) {
	u := &User{ID: uuid.Must(uuid.NewV7()), Email: email, DisplayName: displayName, IsAdmin: isAdmin, Status: StatusPending}
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO users (id, email, display_name, is_admin) VALUES ($1,$2,$3,$4)
			 RETURNING created_at`, u.ID, email, displayName, isAdmin).Scan(&u.CreatedAt)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO credentials (user_id) VALUES ($1)`, u.ID)
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE email = $1`, email))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	return u, err
}

func (s *Store) UserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	return u, err
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetStatus updates user status; on suspended/offboarded it also bumps the
// credential version so every session dies at next validation.
// SetStatus 更新用户状态；suspend/offboard 同时提升凭证版本，使所有会话失效。
func (s *Store) SetStatus(ctx context.Context, id uuid.UUID, status string, offboardedTo *uuid.UUID) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE users SET status=$2, offboarded_to=$3, updated_at=now() WHERE id=$1`,
			id, status, offboardedTo)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrUserNotFound
		}
		if status == StatusSuspended || status == StatusOffboarded {
			if _, err := tx.Exec(ctx,
				`UPDATE credentials SET version = version + 1 WHERE user_id=$1`, id); err != nil {
				return err
			}
			_, err = tx.Exec(ctx,
				`UPDATE sessions SET revoked_at = now() WHERE user_id=$1 AND revoked_at IS NULL`, id)
			return err
		}
		return nil
	})
}

// SetPassword stores a new argon2id hash and bumps credential version,
// killing all existing sessions.
// SetPassword 写入新哈希并提升凭证版本，作废旧会话。
func (s *Store) SetPassword(ctx context.Context, id uuid.UUID, phc string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE credentials SET password_hash=$2, password_changed_at=now(),
			must_change=false, version=version+1 WHERE user_id=$1`, id, phc)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id=$1 AND revoked_at IS NULL`, id)
	return err
}

// ActivateUser flips a pending user to active (called after password is set).
func (s *Store) ActivateUser(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET status='active', updated_at=now() WHERE id=$1 AND status='pending'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotActive
	}
	return nil
}

// PasswordAuth returns what login needs: hash, totp flag, cred version.
func (s *Store) PasswordAuth(ctx context.Context, id uuid.UUID) (hash string, totpEnabled bool, version int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT password_hash, totp_enabled, version FROM credentials WHERE user_id=$1`, id).
		Scan(&hash, &totpEnabled, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrUserNotFound
	}
	return
}

// ---------- known devices / 已知设备 ----------
//
// A device token only skips the account-wide login queue — it is NOT a
// credential and does not bypass the password check or the per-source
// hard lock. Each row binds one random token (stored hashed) to the
// credentials.version at issue, so a password change/reset kills every
// outstanding marker. 设备令牌只豁免账号级登录排队——不是凭证，不绕过
// 密码校验与按来源硬锁。每行绑定一个随机令牌（存哈希）与签发时的凭证
// 版本——改密/重置即全部作废。

// knownDeviceMax per user; knownDeviceTTL after which unused markers die.
const (
	knownDeviceMax = 32
	knownDeviceTTL = "90 days"
)

// IssueDevice mints a fresh device token for uid and stores its hash with
// the current cred version; old markers past the cap/TTL are pruned in
// the same tx. The raw token is returned once — only its hash persists.
// 为 uid 签发新设备令牌：哈希+凭证版本入库，同事务修剪超额/过期标记。
// 明文令牌只返回一次，不落库。
func (s *Store) IssueDevice(ctx context.Context, uid uuid.UUID, credVersion int) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := "kd1." + base64.RawURLEncoding.EncodeToString(raw)
	h := sha256.Sum256([]byte(tok))
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO known_devices (user_id, token_hash, cred_version)
			 VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, uid, h[:], credVersion); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM known_devices WHERE user_id=$1 AND (
			   last_seen_at < now() - $2::interval
			   OR ctid NOT IN (SELECT ctid FROM known_devices WHERE user_id=$1
			                   ORDER BY last_seen_at DESC LIMIT $3))`,
			uid, knownDeviceTTL, knownDeviceMax)
		return err
	})
	if err != nil {
		return "", err
	}
	return tok, nil
}

// KnownDevice reports whether tok is a live marker for uid — the hash
// must exist AND carry the current cred version. last_seen slides so
// active devices don't age out. 校验设备令牌：哈希须存在且凭证版本仍
// 为当前值；顺手滑动 last_seen 防活跃设备过期。
func (s *Store) KnownDevice(ctx context.Context, uid uuid.UUID, tok string, credVersion int) bool {
	if !strings.HasPrefix(tok, "kd1.") {
		return false
	}
	h := sha256.Sum256([]byte(tok))
	tag, err := s.pool.Exec(ctx,
		`UPDATE known_devices SET last_seen_at=now()
		 WHERE user_id=$1 AND token_hash=$2 AND cred_version=$3`,
		uid, h[:], credVersion)
	return err == nil && tag.RowsAffected() == 1
}

// UpdateProfile patches display_name and/or avatar (nil = keep).
// UpdateProfile 部分更新昵称/头像（nil 字段保持不变）。
func (s *Store) UpdateProfile(ctx context.Context, id uuid.UUID, displayName *string, avatar *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET display_name=COALESCE($2,display_name),
			avatar_object_id=COALESCE($3,avatar_object_id), updated_at=now() WHERE id=$1`,
		id, displayName, avatar)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
