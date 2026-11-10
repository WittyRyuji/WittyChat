-- docs.sv: server-maintained Yjs state vector of the persisted update
-- log. The doc relay sends it as SyncStep1 so clients answer with only
-- their missing diff — previously every connect pulled the WHOLE doc back
-- into the log (a ~half-cap doc filled the log on ~2 opens).
-- docs.sv：服务端维护的 Yjs 状态向量（已落库日志）。中继把它作为
-- SyncStep1 下发，客户端只回缺失差量——此前每次连接都把整篇文档回灌
-- 进日志（半满文档被打开约两次即写满）。
ALTER TABLE docs ADD COLUMN sv bytea;

-- known_devices: server-issued per-device login markers. A valid token
-- only skips the account-wide login queue (never a credential). cred_version
-- snapshots credentials.version at issue — password change/reset bumps it
-- and every marker dies with it (the old email-HMAC cookie outlived
-- resets and allowed unlimited guessing).
-- known_devices：服务端签发的"已知设备"标记。有效令牌只豁免账号级登录
-- 排队（不是凭证）。cred_version 记录签发时的凭证版本——改密/重置后
-- 全部标记失效（旧的按邮箱 HMAC cookie 在重置后仍有效且可无限猜密）。
CREATE TABLE known_devices (
    user_id      uuid NOT NULL REFERENCES users (id),
    token_hash   bytea NOT NULL,              -- sha256(token); token itself never stored
    cred_version int NOT NULL,                -- dead once credentials.version moves past it
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, token_hash)
);
CREATE INDEX known_devices_seen ON known_devices (last_seen_at);
