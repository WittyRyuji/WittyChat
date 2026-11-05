DROP TABLE IF EXISTS doc_update_archive;
ALTER TABLE object_versions DROP COLUMN IF EXISTS segments;
