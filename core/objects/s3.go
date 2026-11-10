package objects

// S3 backend: presigned multipart uploads straight to the bucket so large
// files never touch server bandwidth. Config comes from env; when absent
// the store stays on local disk and multipart endpoints return 501.
//
// S3 后端：预签名分片直传，大文件不占服务端带宽。
// 未配置 S3 时回落本地盘，分片接口返回 501。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// S3Config from env / S3 配置来自环境变量
type S3Config struct {
	Endpoint  string // e.g. http://localhost:9000 (minio) — empty = AWS
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	Prefix    string // key prefix, e.g. "wittychat/"
	Enabled   bool
}

func S3ConfigFromEnv() *S3Config {
	c := &S3Config{
		Endpoint:  os.Getenv("WITTYCHAT_S3_ENDPOINT"),
		Bucket:    os.Getenv("WITTYCHAT_S3_BUCKET"),
		Region:    envOr("WITTYCHAT_S3_REGION", "us-east-1"),
		AccessKey: os.Getenv("WITTYCHAT_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("WITTYCHAT_S3_SECRET_KEY"),
		Prefix:    envOr("WITTYCHAT_S3_PREFIX", "wittychat/"),
	}
	c.Enabled = c.Bucket != "" && c.AccessKey != ""
	return c
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// S3 wraps the SDK client + presigner.
type S3 struct {
	cfg     *S3Config
	client  *s3.Client
	presign *s3.PresignClient
}

// EnableS3 attaches an s3 backend; nil keeps local-only behavior.
func (s *Store) EnableS3(s3b *S3) { s.s3 = s3b }

func NewS3(cfg *S3Config, dataDir string) *S3 {
	if !cfg.Enabled {
		return nil
	}
	cli := s3.New(s3.Options{
		Region:       cfg.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		BaseEndpoint: aws.String(cfg.Endpoint),
		UsePathStyle: cfg.Endpoint != "", // minio needs path-style / minio 用路径风格
	})
	return &S3{cfg: cfg, client: cli, presign: s3.NewPresignClient(cli)}
}

func (s *S3) key(sha [32]byte) string {
	return fmt.Sprintf("%s%x", s.cfg.Prefix, sha[:4]) + "/" + HexSHA(sha)
}

// MultipartUpload tracks one open upload (upload_sessions row + s3 id).
type MultipartUpload struct {
	ObjectID  uuid.UUID
	UploadID  string
	PartSize  int32
	ExpiresAt time.Time
}

const S3PartSize = 8 << 20 // 8MB parts

var ErrS3Disabled = errors.New("s3 not configured / 未配置 S3")

// BeginMultipart creates the object row (pending) + s3 multipart upload.
// sha256 is client-declared for dedup pre-check; verified post-complete.
func (s *Store) BeginMultipart(ctx context.Context, owner, convID uuid.UUID,
	purpose, name, mime string, size int64, declaredSHA [32]byte,
) (*MultipartUpload, []string, error) {
	if s.s3 == nil {
		return nil, nil, ErrS3Disabled
	}
	// Dedup: same sha already stored → no upload needed at all.
	var existingID uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM objects WHERE sha256=$1 AND status<>'deleted' LIMIT 1`,
		declaredSHA[:]).Scan(&existingID)
	if err == nil {
		return &MultipartUpload{ObjectID: existingID}, nil, nil // dedup hit
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}

	objID := uuid.Must(uuid.NewV7())
	out, err := s.s3.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.s3.cfg.Bucket),
		Key:         aws.String(s.s3.key(declaredSHA)),
		ContentType: aws.String(mime),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("s3 create multipart: %w", err)
	}
	parts := int((size + S3PartSize - 1) / S3PartSize)
	if parts < 1 {
		parts = 1
	}
	urls := make([]string, parts)
	for i := 1; i <= parts; i++ {
		pre, err := s.s3.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(s.s3.cfg.Bucket),
			Key:        aws.String(s.s3.key(declaredSHA)),
			UploadId:   out.UploadId,
			PartNumber: aws.Int32(int32(i)),
		}, s3.WithPresignExpires(30*time.Minute))
		if err != nil {
			return nil, nil, fmt.Errorf("presign part %d: %w", i, err)
		}
		urls[i-1] = pre.URL
	}
	up := &MultipartUpload{
		ObjectID: objID, UploadID: *out.UploadId,
		PartSize: S3PartSize, ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO objects (id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'s3','pending')`,
			objID, owner, convID, purpose, name, size, declaredSHA[:], mime); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO upload_sessions (id, object_id, user_id, s3_upload_id, part_size, expires_at)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			uuid.Must(uuid.NewV7()), objID, owner, *out.UploadId, S3PartSize, up.ExpiresAt)
		return err
	})
	if err != nil {
		_, _ = s.s3.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(s.s3.key(declaredSHA)),
			UploadId: out.UploadId,
		})
		return nil, nil, err
	}
	return up, urls, nil
}

// CompleteMultipart finishes the upload, verifies size, marks ready.
// etags: [{PartNumber, ETag}] from the client's successful part PUTs.
func (s *Store) CompleteMultipart(ctx context.Context, objID, userID uuid.UUID,
	etags []s3types.CompletedPart,
) (*Object, error) {
	var obj Object
	var s3UpID string
	var sha []byte
	err := s.pool.QueryRow(ctx,
		`SELECT o.id, o.owner_user, o.conv_id, o.purpose, o.name, o.size_bytes, o.sha256,
		        o.mime, o.location, o.status, o.created_at, u.s3_upload_id
		 FROM objects o JOIN upload_sessions u ON u.object_id=o.id
		 WHERE o.id=$1 AND u.state='open'`, objID).
		Scan(&obj.ID, &obj.Owner, &obj.ConvID, &obj.Purpose, &obj.Name, &obj.Size,
			&sha, &obj.Mime, &obj.Location, &obj.Status, &obj.CreatedAt, &s3UpID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if obj.Owner != userID {
		return nil, ErrForbidden
	}
	copy(obj.SHA256[:], sha)
	key := s.s3.key(obj.SHA256)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Hold the key's advisory lock across assemble + reference insert
		// so a GC can't delete the fresh object before the segment row
		// exists. / 持 key 的 advisory lock 覆盖组装与建引用——GC 不能
		// 在分段行建立前删掉新对象。
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
			return err
		}
		if _, err := s.s3.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket:          aws.String(s.s3.cfg.Bucket),
			Key:             aws.String(key),
			UploadId:        aws.String(s3UpID),
			MultipartUpload: &s3types.CompletedMultipartUpload{Parts: etags},
		}); err != nil {
			return fmt.Errorf("s3 complete: %w", err)
		}
		// Verify the assembled object's size matches declaration.
		head, err := s.s3.client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(key),
		})
		if err != nil {
			return fmt.Errorf("s3 head: %w", err)
		}
		if aws.ToInt64(head.ContentLength) != obj.Size {
			return fmt.Errorf("size mismatch: declared %d got %d", obj.Size, aws.ToInt64(head.ContentLength))
		}
		// status='pending' guard: a purge/GC may have soft-deleted the row
		// mid-upload — completing must not resurrect it.
		// 只从 pending 转 ready：上传途中被清理的对象不能复活。
		tag, err := tx.Exec(ctx,
			`UPDATE objects SET status='ready' WHERE id=$1 AND status='pending'`, objID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx,
			`UPDATE upload_sessions SET state='done' WHERE object_id=$1`, objID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO object_segments (object_id, seg_index, size_bytes, storage_key)
			 VALUES ($1,0,$2,$3)`, objID, obj.Size, key)
		return err
	})
	if err != nil {
		return nil, err
	}
	obj.Status = "ready"
	return &obj, nil
}

// OpenS3 streams an s3-resident object.
func (s *Store) openS3(ctx context.Context, o *Object) (io.ReadCloser, error) {
	var key string
	if err := s.pool.QueryRow(ctx,
		`SELECT storage_key FROM object_segments WHERE object_id=$1 ORDER BY seg_index LIMIT 1`,
		o.ID).Scan(&key); err != nil {
		return nil, err
	}
	out, err := s.s3.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	_, _ = s.pool.Exec(ctx, `UPDATE objects SET last_accessed_at=now() WHERE id=$1`, o.ID)
	return out.Body, nil
}
