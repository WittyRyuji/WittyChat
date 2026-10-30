-- 000002 Phase 2-4 schema additions / 第二阶段起补的表与索引

-- trigram index for CJK-friendly substring search on message bodies
-- pg_trgm:中文子串搜索友好(body ILIKE '%kw%')
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX conv_ops_body_trgm ON conv_ops USING gin (body gin_trgm_ops)
    WHERE kind = 'message' AND body IS NOT NULL;

-- widen op-kind set: rename/transfer are conversation-level ops
ALTER TABLE conv_ops DROP CONSTRAINT conv_ops_kind_check;
ALTER TABLE conv_ops ADD CONSTRAINT conv_ops_kind_check CHECK (kind IN
    ('message', 'edit', 'recall', 'member_add', 'member_remove', 'role_change',
     'rename', 'transfer'));

-- ============ S3 multipart upload sessions / S3 分片上传会话 ============
CREATE TABLE upload_sessions (
    id          uuid PRIMARY KEY,
    object_id   uuid NOT NULL REFERENCES objects (id),
    user_id     uuid NOT NULL REFERENCES users (id),
    s3_upload_id text NOT NULL,                           -- provider multipart id
    part_size   int NOT NULL,
    part_etags  jsonb NOT NULL DEFAULT '[]',              -- [{part,etag}] accumulates
    state       text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'done', 'aborted')),
    expires_at  timestamptz NOT NULL,                     -- abandoned sessions GC'd
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX upload_sessions_gc ON upload_sessions (expires_at) WHERE state = 'open';

-- ============ mail / 邮件 ============
CREATE TABLE mail_addresses (                             -- user ↔ address (primary + aliases)
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id),
    address    citext NOT NULL,
    kind       text NOT NULL DEFAULT 'primary' CHECK (kind IN ('primary', 'alias')),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX mail_addresses_addr ON mail_addresses (address);
CREATE INDEX mail_addresses_user ON mail_addresses (user_id);

CREATE TABLE mail_folders (
    id        uuid PRIMARY KEY,
    user_id   uuid NOT NULL REFERENCES users (id),
    kind      text NOT NULL CHECK (kind IN
        ('inbox', 'sent', 'drafts', 'spam', 'archive', 'trash', 'custom')),
    name      text NOT NULL,                              -- display name (custom) or label
    position  int NOT NULL DEFAULT 0,
    UNIQUE (user_id, kind, name)
);
CREATE INDEX mail_folders_user ON mail_folders (user_id);

CREATE TABLE mail_messages (
    id            uuid PRIMARY KEY,
    owner_user_id uuid NOT NULL REFERENCES users (id),    -- whose mailbox copy
    folder_id     uuid NOT NULL REFERENCES mail_folders (id),
    direction     text NOT NULL CHECK (direction IN ('in', 'out')),
    rfc_msgid     text,                                   -- Message-ID header
    thread_id     text,                                   -- normalized subject/References root
    in_reply_to   text,
    from_addr     citext NOT NULL,
    to_addrs      text[] NOT NULL DEFAULT '{}',
    cc_addrs      text[] NOT NULL DEFAULT '{}',
    subject       text NOT NULL DEFAULT '',
    text_body     text NOT NULL DEFAULT '',
    preview       text NOT NULL DEFAULT '',               -- first ~140 chars
    raw_object_id uuid REFERENCES objects (id),           -- raw .eml in objects
    is_read       boolean NOT NULL DEFAULT false,
    is_starred    boolean NOT NULL DEFAULT false,
    size_bytes    bigint NOT NULL DEFAULT 0,
    search_text   text NOT NULL DEFAULT '',               -- subject+body+addrs+attachment names
    search_tsv    tsvector GENERATED ALWAYS AS (to_tsvector('simple', search_text)) STORED,
    sent_at       timestamptz,
    received_at   timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mail_msg_owner ON mail_messages (owner_user_id, folder_id, received_at DESC);
CREATE INDEX mail_msg_thread ON mail_messages (owner_user_id, thread_id, received_at);
CREATE INDEX mail_msg_fts ON mail_messages USING gin (search_tsv);
CREATE INDEX mail_msg_trgm ON mail_messages USING gin (search_text gin_trgm_ops);

CREATE TABLE mail_attachments (
    message_id uuid NOT NULL REFERENCES mail_messages (id) ON DELETE CASCADE,
    object_id  uuid NOT NULL REFERENCES objects (id),
    name       text NOT NULL,
    mime       text,
    size_bytes bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (message_id, object_id)
);

-- ensure every user gets default folders lazily (app-side); nothing else needed.
