package auth

// One-time activation/reset links: only token hash is stored; each link is
// single-use and expires (default 72h). Issuing a new link kills prior
// unused ones of the same kind for that user.
//
// 一次性激活/重置链接：只存哈希、单次使用、默认 72 小时。
// 签发新链接会作废旧未用链接（同用户同类型）。

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const LinkTTL = 72 * time.Hour

const (
	LinkKindActivation = "activation"
	LinkKindReset      = "reset"
)

var ErrLinkInvalid = errors.New("link invalid, used, or expired")

// CreateActivationLink issues a one-time link for userID.
// CreateActivationLink 为指定用户签发一次性链接。
func (s *Store) CreateActivationLink(ctx context.Context, userID uuid.UUID, kind string, createdBy uuid.UUID) (token string, expiresAt time.Time, err error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", time.Time{}, err
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE activation_links SET used_at = now()
			 WHERE user_id=$1 AND kind=$2 AND used_at IS NULL`, userID, kind); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO activation_links (token_hash, user_id, kind, expires_at, created_by)
			 VALUES ($1,$2,$3, now()+$4::interval, $5) RETURNING expires_at`,
			hash, userID, kind, LinkTTL, createdBy).Scan(&expiresAt)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// PeekLink returns link info without consuming it (for the activation page).
func (s *Store) PeekLink(ctx context.Context, token string) (email, displayName string, expiresAt time.Time, kind string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT u.email, u.display_name, l.expires_at, l.kind
		 FROM activation_links l JOIN users u ON u.id = l.user_id
		 WHERE l.token_hash=$1 AND l.used_at IS NULL AND l.expires_at > now()`,
		HashToken(token)).Scan(&email, &displayName, &expiresAt, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrLinkInvalid
	}
	return
}

// ConsumeLink validates + marks used, returning the user it belongs to.
// ConsumeLink 校验并标记已用，返回所属用户。
func (s *Store) ConsumeLink(ctx context.Context, token string) (userID uuid.UUID, kind string, err error) {
	err = s.pool.QueryRow(ctx,
		`UPDATE activation_links SET used_at = now()
		 WHERE token_hash=$1 AND used_at IS NULL AND expires_at > now()
		 RETURNING user_id, kind`,
		HashToken(token)).Scan(&userID, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrLinkInvalid
	}
	return
}
