DROP TABLE IF EXISTS doc_objects;
ALTER TABLE docs DROP COLUMN IF EXISTS import_payload, DROP COLUMN IF EXISTS import_claimed_at, DROP COLUMN IF EXISTS import_source;
