package auth

// Device sessions: one row per login, revocable, idle-expiring, killed on
// password change / deactivation via credential version.
//
// 设备会话：每次登录一行，可吊销，闲置过期，改密/停用经凭证版本全部踢下线。

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	SessionIdleTTL      = 30 * 24 * time.Hour
	refreshSeenThrottle = 5 * time.Minute
)

var ErrSessionInvalid = errors.New("session invalid or expired")

type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	DeviceName string
	DeviceKind string
	LastSeenAt time.Time
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// CreateSession inserts a session row and returns the bearer token + expiry.
// CreateSession 建会话行并返回令牌与过期时间。哈希入库，令牌只此一次可见。
func (s *Store) CreateSession(ctx context.Context, userID uuid.UUID, deviceName, deviceKind string, credVersion int) (token string, sess *Session, err error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", nil, err
	}
	sess = &Session{ID: uuid.Must(uuid.NewV7()), UserID: userID, DeviceName: deviceName, DeviceKind: deviceKind}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO sessions (id, user_id, device_name, device_kind, credential_version, token_hash, idle_expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6, now() + $7::interval)
		 RETURNING idle_expires_at, created_at`,
		sess.ID, userID, deviceName, deviceKind, credVersion, hash, SessionIdleTTL).
		Scan(&sess.ExpiresAt, &sess.CreatedAt)
	if err != nil {
		return "", nil, err
	}
	return token, sess, nil
}

// ValidateSession checks token → session validity in one query:
// not revoked, not idle-expired, credential version still current, user active.
// Sliding window: refreshes last_seen/idle expiry at most every 5min.
//
// ValidateSession 单查询校验会话：未吊销、未闲置过期、凭证版本仍有效、用户仍 active。
// 滑动窗口：每 5 分钟最多刷新一次活跃时间。
func (s *Store) ValidateSession(ctx context.Context, token string) (*Session, error) {
	hash := HashToken(token)
	var sess Session
	var credVersion int
	var userVersion int
	var userStatus string
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.user_id, COALESCE(s.device_name,''), COALESCE(s.device_kind,''),
		        s.last_seen_at, s.idle_expires_at, s.created_at, s.credential_version,
		        c.version, u.status
		 FROM sessions s
		 JOIN credentials c ON c.user_id = s.user_id
		 JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1 AND s.revoked_at IS NULL`, hash).
		Scan(&sess.ID, &sess.UserID, &sess.DeviceName, &sess.DeviceKind,
			&sess.LastSeenAt, &sess.ExpiresAt, &sess.CreatedAt, &credVersion,
			&userVersion, &userStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionInvalid
		}
		return nil, err
	}
	if time.Now().After(sess.ExpiresAt) || credVersion != userVersion || userStatus != StatusActive {
		return nil, ErrSessionInvalid
	}

	// Slide idle expiry at most every refreshSeenThrottle.
	if time.Since(sess.LastSeenAt) > refreshSeenThrottle {
		_, _ = s.pool.Exec(ctx,
			`UPDATE sessions SET last_seen_at=now(), idle_expires_at=now()+$2::interval
			 WHERE id=$1`, sess.ID, SessionIdleTTL)
		sess.LastSeenAt = time.Now()
		sess.ExpiresAt = sess.LastSeenAt.Add(SessionIdleTTL)
	}
	return &sess, nil
}

func (s *Store) ListSessions(ctx context.Context, userID uuid.UUID) ([]*Session, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, COALESCE(device_name,''), COALESCE(device_kind,''),
		        last_seen_at, idle_expires_at, created_at
		 FROM sessions WHERE user_id=$1 AND revoked_at IS NULL
		 ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		var s0 Session
		if err := rows.Scan(&s0.ID, &s0.UserID, &s0.DeviceName, &s0.DeviceKind,
			&s0.LastSeenAt, &s0.ExpiresAt, &s0.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &s0)
	}
	return out, rows.Err()
}

// RevokeSession revokes one session owned by userID.
func (s *Store) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`,
		sessionID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionInvalid
	}
	return nil
}
