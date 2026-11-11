-- down: drop upload_sessions.s3_key / 回滚：删除 upload_sessions.s3_key
ALTER TABLE upload_sessions DROP COLUMN s3_key;
