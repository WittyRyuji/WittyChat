-- Speed up the shared-file reference checks every delete path runs:
-- object_segments.storage_key equality probes and the object_versions
-- segments jsonb containment probe. Without these each dropKeyIfOrphaned
-- scans both tables — quadratic as retained versions accumulate.
-- 为删除路径的共享文件引用检查建索引：object_segments.storage_key 等值
-- 查询 + object_versions.segments 的 jsonb 包含查询。没有索引时每次
-- dropKeyIfOrphaned 都全扫两表——版本一多就成平方级。
CREATE INDEX IF NOT EXISTS object_segments_key_idx
    ON object_segments (storage_key);

CREATE INDEX IF NOT EXISTS object_versions_segments_gin
    ON object_versions USING gin (segments jsonb_path_ops);
