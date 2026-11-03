-- Doc kinds: doc (rich text), sheet (spreadsheet w/ formulas), slide
-- (presentation). Content stays Yjs update blobs regardless of kind.
-- 文档类型：doc 富文本 / sheet 表格（含公式）/ slide 演示。内容仍是
-- Yjs update 帧，不随类型变化。
-- IF NOT EXISTS: dev DBs that had this applied by hand stay migratable.
ALTER TABLE docs
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'doc'
    CHECK (kind IN ('doc', 'sheet', 'slide'));
