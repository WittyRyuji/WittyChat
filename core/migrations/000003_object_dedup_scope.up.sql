-- Dedup scope: same uploader+conversation may share one object row per sha;
-- different owners/conversations need separate rows for correct ownership/ACL.
-- Identical bytes still share the sha-named file on disk.
-- 秒退去重收窄到 同主+同会话；跨主/跨会话各自建行保证归属与 ACL 正确，
-- 磁盘文件按 sha 命名仍然去重。
DROP INDEX objects_sha256;

CREATE UNIQUE INDEX objects_sha256_scoped ON objects
    (sha256, owner_user, COALESCE(conv_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE status <> 'deleted';
