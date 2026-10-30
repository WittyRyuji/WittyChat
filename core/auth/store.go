// Package auth implements the identity domain: users, credentials, sessions,
// activation links, TOTP, login throttling.
//
// auth 包实现身份域：用户、凭证、会话、激活链接、二次验证、登录限速。
package auth

import (
	"context"
	"errors"
	"fmt"
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
