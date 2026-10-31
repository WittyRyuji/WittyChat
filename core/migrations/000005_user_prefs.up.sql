-- Per-user preferences KV — mail trust list, image autoload, UI toggles.
-- Cross-device "personal info" (stickers stay per-device by design).
-- 用户级偏好键值：邮件信任/自动载图/界面开关——跨设备个人信息。
CREATE TABLE user_prefs (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key        text NOT NULL,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);
