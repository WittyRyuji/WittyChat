-- Hardening after the docs/office review.
-- 文档/Office 审查后的加固。

-- Docs: soft delete — deleted docs (and their Yjs history) are retained for
-- audit but invisible, matching "撤回消息附件留审计不可见".
-- 文档软删除：删除后连同 Yjs 历史保留供审计，但对用户不可见。
ALTER TABLE docs ADD COLUMN deleted_at timestamptz;
ALTER TABLE docs ADD COLUMN deleted_by uuid REFERENCES users (id);

-- Docs shared into a conversation (sending a doc link grants that conv's
-- members access, Feishu-style). Bound conv (docs.conv_id) stays primary.
-- 文档分享到会话：发送文档链接即授予该会话成员访问权（飞书式）。
CREATE TABLE doc_shares (
    doc_id     uuid NOT NULL REFERENCES docs (id) ON DELETE CASCADE,
    conv_id    uuid NOT NULL REFERENCES conversations (id),
    shared_by  uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_id, conv_id)
);
CREATE INDEX doc_shares_conv_idx ON doc_shares (conv_id);

-- Prior bytes of objects overwritten in place (OnlyOffice save) — the old
-- file stays on disk/S3 and is addressable here for audit/restore.
-- 原位覆盖（OnlyOffice 保存）前的旧版本：旧文件保留并在此可寻址，供审计/恢复。
CREATE TABLE object_versions (
    object_id   uuid NOT NULL REFERENCES objects (id),
    version     int NOT NULL,
    sha256      bytea NOT NULL,
    size_bytes  bigint NOT NULL,
    mime        text NOT NULL DEFAULT '',
    location    text NOT NULL,
    storage_key text NOT NULL,
    replaced_by uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (object_id, version)
);

-- Own sent copies used to be inserted unread; the unread badge now only
-- counts inbound mail, so fix the stale flags too.
-- 历史发件副本曾以未读入库；角标已只计收件方向，这里一并修正旧标记。
UPDATE mail_messages SET is_read = true WHERE direction = 'out' AND NOT is_read;
