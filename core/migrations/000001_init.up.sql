-- WittyChat initial schema / 初始 schema
-- IDs are UUIDv7 generated app-side (time-ordered, offline-safe, federation-ready).
-- ID 由应用侧生成 UUIDv7（时间有序、离线可生成、联邦不冲突）。

CREATE EXTENSION IF NOT EXISTS citext;

-- ============ identity / 身份 ============

CREATE TABLE users (
    id               uuid PRIMARY KEY,
    email            citext NOT NULL,                     -- name@domain = login = IM addr = mail addr
    display_name     text NOT NULL,
    status           text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'active', 'suspended', 'offboarded')),
    is_admin         boolean NOT NULL DEFAULT false,      -- workspace admin (Team Owner)
    avatar_object_id uuid,
    offboarded_to    uuid REFERENCES users (id),          -- admin who received handover
    locale           text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (email);

CREATE TABLE credentials (
    user_id             uuid PRIMARY KEY REFERENCES users (id),
    password_hash       text NOT NULL DEFAULT '',         -- argon2id encoded; empty until activation
    password_changed_at timestamptz NOT NULL DEFAULT now(),
    must_change         boolean NOT NULL DEFAULT false,
    totp_secret         bytea,                            -- encrypted at rest / 落库前加密
    totp_enabled        boolean NOT NULL DEFAULT false,
    version             int NOT NULL DEFAULT 1            -- bump → all sessions invalid / 改密后全部会话失效
);

CREATE TABLE activation_links (
    token_hash  bytea PRIMARY KEY,                        -- sha256(token); token itself never stored
    user_id     uuid NOT NULL REFERENCES users (id),
    kind        text NOT NULL CHECK (kind IN ('activation', 'reset')),
    expires_at  timestamptz NOT NULL,                     -- default 72h set app-side
    used_at     timestamptz,
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX activation_links_user ON activation_links (user_id) WHERE used_at IS NULL;

CREATE TABLE sessions (
    id                 uuid PRIMARY KEY,
    user_id            uuid NOT NULL REFERENCES users (id),
    device_name        text,
    device_kind        text,                              -- desktop / web / mobile
    credential_version int NOT NULL,                      -- invalid when credentials.version > this
    token_hash         bytea NOT NULL UNIQUE,
    last_seen_at       timestamptz NOT NULL DEFAULT now(),
    idle_expires_at    timestamptz NOT NULL,              -- now + 30d, slid on activity
    revoked_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user ON sessions (user_id) WHERE revoked_at IS NULL;

-- ============ conversations & ops / 会话与操作流 ============

CREATE TABLE conversations (
    id          uuid PRIMARY KEY,
    kind        text NOT NULL CHECK (kind IN ('dm', 'group', 'assistant')),
    title       text,
    is_public   boolean NOT NULL DEFAULT false,           -- public group: searchable, joinable
    owner_id    uuid REFERENCES users (id),
    dm_key      text,                                     -- 'min_uid:max_uid' → dm dedup
    last_op_seq bigint NOT NULL DEFAULT 0,                -- per-conv op counter, ++ in same tx
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX conversations_dm_key ON conversations (dm_key) WHERE dm_key IS NOT NULL;
CREATE INDEX conversations_public ON conversations (id) WHERE is_public;

CREATE TABLE conv_members (
    conv_id          uuid NOT NULL REFERENCES conversations (id),
    user_id          uuid NOT NULL REFERENCES users (id),
    role             text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member')),
    last_read_op_seq bigint NOT NULL DEFAULT 0,           -- read watermark / 已读位置
    joined_at        timestamptz NOT NULL DEFAULT now(),
    left_at          timestamptz,
    PRIMARY KEY (conv_id, user_id)
);
CREATE INDEX conv_members_user ON conv_members (user_id) WHERE left_at IS NULL;

-- One op stream per conversation: message / edit / recall / member changes.
-- 每个会话一条 op 流：消息/编辑/撤回/成员变更。消息本体就是 message op。
CREATE TABLE conv_ops (
    conv_id            uuid NOT NULL REFERENCES conversations (id),
    op_seq             bigint NOT NULL,
    op_id              uuid NOT NULL,                     -- UUIDv7
    kind               text NOT NULL CHECK (kind IN
        ('message', 'edit', 'recall', 'member_add', 'member_remove', 'role_change')),
    actor_id           uuid REFERENCES users (id),
    actor_assistant_id uuid,                              -- set when sent via an assistant
    client_msg_id      uuid,                              -- idempotency / 幂等
    target_op_seq      bigint,                            -- edit/recall → which op
    body               text,                              -- denormalized text for preview+search
    payload            jsonb NOT NULL DEFAULT '{}',       -- markdown, mentions, quote, object ids…
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (conv_id, op_seq)
);
CREATE UNIQUE INDEX conv_ops_op_id ON conv_ops (op_id);
CREATE UNIQUE INDEX conv_ops_client_msg ON conv_ops (client_msg_id) WHERE client_msg_id IS NOT NULL;
CREATE INDEX conv_ops_body_fts ON conv_ops USING gin (to_tsvector('simple', body))
    WHERE kind = 'message' AND body IS NOT NULL;

-- ============ objects / 统一对象存储 ============

CREATE TABLE objects (
    id               uuid PRIMARY KEY,
    owner_user       uuid REFERENCES users (id),
    conv_id          uuid REFERENCES conversations (id),
    purpose          text,                                -- attachment/mail_raw/transcript/export/thumb
    name             text,
    size_bytes       bigint NOT NULL,
    sha256           bytea NOT NULL,
    mime             text,
    location         text NOT NULL DEFAULT 'local' CHECK (location IN ('local', 's3')),
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'deleted')),
    segment_count    int NOT NULL DEFAULT 1,
    last_accessed_at timestamptz NOT NULL DEFAULT now(),  -- cold-migration signal
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX objects_sha256 ON objects (sha256) WHERE status <> 'deleted';  -- dedup / 秒传

CREATE TABLE object_segments (
    object_id   uuid NOT NULL REFERENCES objects (id),
    seg_index   int NOT NULL,
    size_bytes  int NOT NULL,                             -- each <= 2MB enforced app-side
    storage_key text NOT NULL,                            -- disk path or s3 key
    PRIMARY KEY (object_id, seg_index)
);

CREATE TABLE attachment_refs (                            -- message ↔ object linkage
    conv_id    uuid NOT NULL,
    op_seq     bigint NOT NULL,
    object_id  uuid NOT NULL REFERENCES objects (id),
    PRIMARY KEY (conv_id, op_seq, object_id),
    FOREIGN KEY (conv_id, op_seq) REFERENCES conv_ops (conv_id, op_seq)
);

-- ============ tasks / audit / assistants / settings ============

CREATE TABLE bg_tasks (                                   -- worker queue / worker 任务表
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind        text NOT NULL,                            -- thumb/gc/migrate/mail_parse/…
    payload     jsonb NOT NULL DEFAULT '{}',
    status      text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    attempts    int NOT NULL DEFAULT 0,
    run_after   timestamptz NOT NULL DEFAULT now(),
    locked_by   text,
    locked_at   timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX bg_tasks_pick ON bg_tasks (run_after) WHERE status = 'queued';

CREATE TABLE audit_events (
    id         bigint GENERATED ALWAYS AS IDENTITY,
    actor_id   uuid,
    actor_kind text NOT NULL DEFAULT 'user',              -- user/admin/assistant/system/hook
    action     text NOT NULL,
    target     text,
    detail     jsonb NOT NULL DEFAULT '{}',
    ip         inet,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)                          -- partition-ready / 便于按月分区
);
CREATE INDEX audit_events_time ON audit_events (created_at);

CREATE TABLE assistants (                                 -- P0: config only / P0 只建结构
    id               uuid PRIMARY KEY,
    owner_id         uuid NOT NULL REFERENCES users (id),
    name             text NOT NULL,
    avatar_object_id uuid,
    runtime          text NOT NULL DEFAULT 'api' CHECK (runtime IN ('acp', 'api')),
    adapter          text,                                -- codex / qwen / claude / openai-compat…
    model            text,
    instructions     text,
    scopes           jsonb NOT NULL DEFAULT '[]',         -- granted capability scopes
    session_level    smallint NOT NULL DEFAULT 2 CHECK (session_level BETWEEN 1 AND 4),
    auto_send        boolean NOT NULL DEFAULT false,      -- 代发免确认（默认逐条确认）
    status           text NOT NULL DEFAULT 'disabled',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workspace_settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_by uuid REFERENCES users (id),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Low-frequency global events: contacts / settings / assistant config.
-- 全局低频事件流：联系人、设置、助理配置等。
CREATE TABLE global_events (
    seq        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       text NOT NULL,
    payload    jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
