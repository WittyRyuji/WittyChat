-- 16: upload_sessions.s3_key — multipart uploads now target a per-upload
-- key (up/<object-id>), not the content-addressed sha key, which killed the
-- declared-sha key-collision attack. NULL = legacy row, key falls back to
-- s3.key(objects.sha256).
-- 16：upload_sessions.s3_key——分片上传改用上载专属 key（up/<对象id>），
-- 不再用按内容寻址的 sha key，杜绝声明 sha 撞库攻击。NULL 为旧行，
-- key 回退按 objects.sha256 计算。
ALTER TABLE upload_sessions ADD COLUMN s3_key text;
