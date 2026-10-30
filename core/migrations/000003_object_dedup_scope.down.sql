DROP INDEX objects_sha256_scoped;

CREATE UNIQUE INDEX objects_sha256 ON objects (sha256) WHERE status <> 'deleted';
