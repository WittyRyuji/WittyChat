// Package apitypes defines the public API contract shared by server and clients.
// Types use huma tags so the server can generate OpenAPI from them.
//
// apitypes 包定义服务端与客户端共用的公开 API 契约。
// 类型带 huma 标签，服务端据此生成 OpenAPI。
package apitypes

import "time"

// ---------- auth / 认证 ----------

type LoginRequest struct {
	Email      string `json:"email" required:"true" format:"email"`
	Password   string `json:"password" required:"true" minLength:"1"`
	DeviceName string `json:"device_name,omitempty" maxLength:"128"`
	DeviceKind string `json:"device_kind,omitempty" enum:"desktop,web,mobile"`
	TOTP       string `json:"totp,omitempty" pattern:"^[0-9]{6}$"`
}

type UserPublic struct {
	ID             string    `json:"id"`
	Email          string    `json:"email"`
	DisplayName    string    `json:"display_name"`
	Status         string    `json:"status" enum:"pending,active,suspended,offboarded"`
	IsAdmin        bool      `json:"is_admin"`
	TOTPEnabled    bool      `json:"totp_enabled"`
	AvatarObjectID string    `json:"avatar_object_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type UpdateMeRequest struct {
	DisplayName    string `json:"display_name,omitempty" maxLength:"64"`
	AvatarObjectID string `json:"avatar_object_id,omitempty" format:"uuid"`
}

type LoginResponse struct {
	SessionToken string      `json:"session_token,omitempty"`
	RequiresTOTP bool        `json:"requires_totp,omitempty"`
	User         *UserPublic `json:"user,omitempty"`
	ExpiresAt    time.Time   `json:"expires_at,omitempty"`
}

type ActivateRequest struct {
	Token    string `json:"token" required:"true"`
	Password string `json:"password" required:"true" minLength:"8"`
}

type ActivateInfoResponse struct {
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// ---------- admin / 管理 ----------

type CreateUserRequest struct {
	Email       string `json:"email" required:"true" format:"email"`
	DisplayName string `json:"display_name" required:"true" maxLength:"128"`
	IsAdmin     bool   `json:"is_admin,omitempty"`
}

type CreateUserResponse struct {
	User          *UserPublic `json:"user"`
	ActivationURL string      `json:"activation_url"`
	ExpiresAt     time.Time   `json:"expires_at"`
}

type UserListResponse struct {
	Users []*UserPublic `json:"users"`
}

type OffboardRequest struct {
	TransferTo string `json:"transfer_to" required:"true" format:"uuid" doc:"admin user id who takes over"`
}

// ---------- sessions / 设备会话 ----------

type SessionInfo struct {
	ID         string    `json:"id"`
	DeviceName string    `json:"device_name"`
	DeviceKind string    `json:"device_kind"`
	LastSeenAt time.Time `json:"last_seen_at"`
	CreatedAt  time.Time `json:"created_at"`
	Current    bool      `json:"current"`
}

type SessionListResponse struct {
	Sessions []*SessionInfo `json:"sessions"`
}

// ---------- TOTP / 二次验证 ----------

type TOTPEnrollResponse struct {
	SecretURI string `json:"secret_uri" doc:"otpauth:// URI for authenticator apps"`
}

type TOTPConfirmRequest struct {
	Code string `json:"code" required:"true" pattern:"^[0-9]{6}$"`
}

type TOTPDisableRequest struct {
	Password string `json:"password" required:"true" minLength:"1"`
}
