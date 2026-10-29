package auth

// TOTP (RFC 6238) optional 2FA. Secrets are AES-256-GCM sealed at rest.
// TOTP（RFC 6238）可选二次验证。种子落库前 AES-256-GCM 加密。

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

var (
	ErrTOTPNotEnrolled = errors.New("totp not enrolled")
	ErrTOTPBadCode     = errors.New("invalid totp code")
)

// TOTPEnroll generates a new secret (not yet enabled) and returns the otpauth URI.
// TOTPEnroll 生成新种子（未启用）并返回 otpauth URI。
func (s *Store) TOTPEnroll(ctx context.Context, userID uuid.UUID, email, issuer string, encKey []byte) (uri string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: email,
		Period:      30,
		Digits:      otp.DigitsSix,
	})
	if err != nil {
		return "", err
	}
	sealed, err := SealSecret(encKey, []byte(key.Secret()))
	if err != nil {
		return "", err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE credentials SET totp_secret=$2, totp_enabled=false WHERE user_id=$1`, userID, sealed)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrUserNotFound
	}
	return key.URL(), nil
}

// TOTPConfirm verifies the first code then enables 2FA.
// TOTPConfirm 校验首个验证码后启用 2FA。
func (s *Store) TOTPConfirm(ctx context.Context, userID uuid.UUID, code string, encKey []byte) error {
	secret, err := s.totpSecret(ctx, userID, encKey)
	if err != nil {
		return err
	}
	if !totp.Validate(code, secret) {
		return ErrTOTPBadCode
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE credentials SET totp_enabled=true WHERE user_id=$1`, userID)
	return err
}

// TOTPVerify checks a code against the enrolled secret.
func (s *Store) TOTPVerify(ctx context.Context, userID uuid.UUID, code string, encKey []byte) bool {
	secret, err := s.totpSecret(ctx, userID, encKey)
	if err != nil {
		return false
	}
	return totp.Validate(code, secret)
}

// TOTPDisable clears the secret (caller must re-verify password first).
// TOTPDisable 清除种子（调用方须先验证密码）。
func (s *Store) TOTPDisable(ctx context.Context, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE credentials SET totp_secret=NULL, totp_enabled=false WHERE user_id=$1`, userID)
	return err
}

func (s *Store) totpSecret(ctx context.Context, userID uuid.UUID, encKey []byte) (string, error) {
	var sealed []byte
	err := s.pool.QueryRow(ctx,
		`SELECT totp_secret FROM credentials WHERE user_id=$1 AND totp_secret IS NOT NULL`, userID).
		Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTOTPNotEnrolled
	}
	if err != nil {
		return "", err
	}
	raw, err := OpenSecret(encKey, sealed)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
