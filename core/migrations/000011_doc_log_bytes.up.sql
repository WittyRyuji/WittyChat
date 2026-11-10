-- Running byte total of each doc's update log. The per-keystroke cap
-- check used to SUM the whole log (O(rows) and slower as it grew);
-- maintaining a counter makes it O(1). Compaction recomputes it.
-- 每文档更新日志的运行字节数。此前逐键击的上限检查要 SUM 整条日志
-- （O(行数)且随日志增长变慢）；改为计数器后 O(1)，压缩时重算。

ALTER TABLE docs ADD COLUMN log_bytes bigint NOT NULL DEFAULT 0;

UPDATE docs d SET log_bytes = (
    SELECT COALESCE(SUM(LENGTH(u.data)), 0)
    FROM doc_updates u WHERE u.doc_id = d.id);
