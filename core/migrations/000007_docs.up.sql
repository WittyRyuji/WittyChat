-- Native collaborative documents (Feishu-style). A doc may be bound to a
-- conversation — members of that conv can open/co-edit it in realtime.
-- Content lives in Yjs update blobs, replayed on join.
-- 原生协同文档（飞书式）：可绑定会话，成员实时共编；内容以 Yjs
-- update 帧落库，后进协作者回放全量。

CREATE TABLE docs (
    id         uuid PRIMARY KEY,
    title      text NOT NULL DEFAULT '',
    owner      uuid NOT NULL REFERENCES users (id),
    conv_id    uuid REFERENCES conversations (id),  -- NULL = personal doc
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX docs_owner_idx ON docs (owner, updated_at DESC);
CREATE INDEX docs_conv_idx ON docs (conv_id) WHERE conv_id IS NOT NULL;

-- Append-only Yjs update log. Every editing client produces update blobs;
-- the relay persists them in order and replays to late joiners.
-- 追加式 Yjs 更新日志：客户端产出的 update 帧按序落库，后进者全量回放。
CREATE TABLE doc_updates (
    doc_id uuid NOT NULL REFERENCES docs (id) ON DELETE CASCADE,
    seq    bigint GENERATED ALWAYS AS IDENTITY,
    data   bytea NOT NULL,
    PRIMARY KEY (doc_id, seq)
);
