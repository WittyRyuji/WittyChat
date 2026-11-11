-- 20: staged doc import — a server-stored payload that the first editor to
-- open the doc applies through the real Yjs path. doc_objects links imported
-- images/files to a doc so canReadObject can grant doc-ACL access.
-- 20：文档暂存导入——服务端存载荷，首个打开空文档的客户端经真实 Yjs 路径写入。
ALTER TABLE docs ADD COLUMN import_payload jsonb, ADD COLUMN import_claimed_at timestamptz, ADD COLUMN import_source text;
CREATE TABLE doc_objects (doc_id uuid NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
  object_id uuid NOT NULL REFERENCES objects(id), PRIMARY KEY (doc_id, object_id));
CREATE INDEX doc_objects_obj ON doc_objects (object_id);
