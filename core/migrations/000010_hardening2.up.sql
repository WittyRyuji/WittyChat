-- Hardening round 2: compaction archive + full version segments.
-- 第二轮加固：压缩归档 + 版本全分段记录。

-- Compact no longer deletes Yjs history outright: replaced rows move here
-- for audit and recovery, and are purged after the audit retention window.
-- 压缩不再直接删除 Yjs 历史：被替换的行移入归档表供审计与恢复，
-- 超过审计保留期后清除。
CREATE TABLE doc_update_archive (
    doc_id       uuid NOT NULL,
    seq          bigint NOT NULL,
    data         bytea NOT NULL,
    compacted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (doc_id, seq)
);
CREATE INDEX doc_update_archive_time_idx ON doc_update_archive (compacted_at);

-- A version may span several storage segments (large S3 objects); keep the
-- full segment list so old versions stay restorable and GC can delete the
-- right files. 版本可能跨多个存储段（大对象 S3 分段）；记录全部分段，
-- 旧版本才可恢复、GC 才能删对文件。
ALTER TABLE object_versions ADD COLUMN segments jsonb;
