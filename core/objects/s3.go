package objects

// S3 backend: presigned multipart uploads straight to the bucket so large
// files never touch server bandwidth. Config comes from env; when absent
// the store stays on local disk and multipart endpoints return 501.
//
// S3 后端：预签名分片直传，大文件不占服务端带宽。
// 未配置 S3 时回落本地盘，分片接口返回 501。

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// S3Config from env / S3 配置来自环境变量
type S3Config struct {
	Endpoint  string // e.g. http://localhost:9000 (minio) — empty = AWS
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	Prefix    string // key prefix, e.g. "wittychat/"
	// PublicEndpoint signs presigned URLs for the client-reachable host —
	// in docker deployments the internal endpoint isn't reachable from
	// browsers. SigV4 signs the Host, so the public endpoint must reach S3
	// with that Host preserved.
	// 公开端点：给客户端可访问的 host 签名。docker 部署里内部地址浏览器
	// 不可达；SigV4 对 Host 签名，公开端点须原样转发该 Host。
	PublicEndpoint string
	Enabled        bool
}

func S3ConfigFromEnv() *S3Config {
	c := &S3Config{
		Endpoint:       os.Getenv("WITTYCHAT_S3_ENDPOINT"),
		Bucket:         os.Getenv("WITTYCHAT_S3_BUCKET"),
		Region:         envOr("WITTYCHAT_S3_REGION", "us-east-1"),
		AccessKey:      os.Getenv("WITTYCHAT_S3_ACCESS_KEY"),
		SecretKey:      os.Getenv("WITTYCHAT_S3_SECRET_KEY"),
		Prefix:         envOr("WITTYCHAT_S3_PREFIX", "wittychat/"),
		PublicEndpoint: os.Getenv("WITTYCHAT_S3_PUBLIC_ENDPOINT"),
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
	opts := func(ep string) s3.Options {
		return s3.Options{
			Region:       cfg.Region,
			Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
			BaseEndpoint: aws.String(ep),
			UsePathStyle: ep != "", // minio needs path-style / minio 用路径风格
		}
	}
	cli := s3.New(opts(cfg.Endpoint))
	pre := cli
	if cfg.PublicEndpoint != "" {
		pre = s3.New(opts(cfg.PublicEndpoint))
	}
	return &S3{cfg: cfg, client: cli, presign: s3.NewPresignClient(pre)}
}

func (s *S3) key(sha [32]byte) string {
	return fmt.Sprintf("%s%x", s.cfg.Prefix, sha[:4]) + "/" + HexSHA(sha)
}

// uploadKey is the per-upload key for multipart bytes — unique per object
// id so a client-declared sha can never collide with (or overwrite) some
// other object's content-addressed key.
// 上传专属 key：按对象 id 唯一，声明的 sha 不会与其它对象的寻址 key 相撞。
func (s *S3) uploadKey(id uuid.UUID) string {
	return s.cfg.Prefix + "up/" + id.String()
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
	purpose, name, mime string, size int64, declaredSHA *[32]byte,
) (*MultipartUpload, []string, error) {
	if s.s3 == nil {
		return nil, nil, ErrS3Disabled
	}
	// Dedup hint, scoped like Put (owner+conv+sha): a hit skips the upload.
	// The hint is a courtesy only — a wrong declared sha is ignored at
	// complete, which hashes the real bytes server-side.
	// 去重提示与 Put 同域（属主+会话+sha）；仅作提示——声明错了也在
	// complete 服务端哈希时被忽略。
	if declaredSHA != nil {
		var existingID uuid.UUID
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM objects WHERE sha256=$1 AND owner_user=$2
			   AND conv_id IS NOT DISTINCT FROM $3 AND status='ready'`,
			declaredSHA[:], owner, nilUUID(convID)).Scan(&existingID)
		if err == nil {
			return &MultipartUpload{ObjectID: existingID}, nil, nil // dedup hit
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
	}

	objID := uuid.Must(uuid.NewV7())
	key := s.s3.uploadKey(objID)
	out, err := s.s3.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.s3.cfg.Bucket),
		Key:         aws.String(key),
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
			Key:        aws.String(key),
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
	// Placeholder sha: unique per row so it never trips the scoped sha256
	// unique index; replaced by the server-computed hash at complete.
	// 占位 sha：每行唯一，不会撞上 scoped sha256 唯一索引；complete 时
	// 由服务端实测哈希替换。
	placeholder := sha256.Sum256([]byte("pending:" + objID.String()))
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO objects (id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status, dims_checked)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'s3','pending',$9)`,
			objID, owner, nilUUID(convID), purpose, name, size, placeholder[:], mime,
			!strings.HasPrefix(mime, "image/")); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO upload_sessions (id, object_id, user_id, s3_upload_id, part_size, expires_at, s3_key)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			uuid.Must(uuid.NewV7()), objID, owner, *out.UploadId, S3PartSize, up.ExpiresAt, key)
		return err
	})
	if err != nil {
		_, _ = s.s3.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(key),
			UploadId: out.UploadId,
		})
		return nil, nil, err
	}
	return up, urls, nil
}

// CompleteMultipart finishes the upload, verifies size, hashes the real
// bytes server-side (a declared sha was only a dedup hint — a wrong one is
// ignored), then dedups at complete within owner+conv. Returns dedup=true
// when another ready row already holds the same content.
// etags: [{PartNumber, ETag}] from the client's successful part PUTs.
// complete 服务端实测哈希（声明的 sha 只是提示），再按属主+会话去重。
func (s *Store) CompleteMultipart(ctx context.Context, objID, userID uuid.UUID,
	etags []s3types.CompletedPart,
) (*Object, bool, error) {
	var obj Object
	var s3UpID, sessKey string
	var sessKeyNull *string
	err := s.pool.QueryRow(ctx,
		`SELECT o.id, o.owner_user, o.conv_id, o.purpose, o.name, o.size_bytes,
		        o.mime, o.location, o.status, o.created_at, u.s3_upload_id, u.s3_key
		 FROM objects o JOIN upload_sessions u ON u.object_id=o.id
		 WHERE o.id=$1 AND u.state='open'`, objID).
		Scan(&obj.ID, &obj.Owner, &obj.ConvID, &obj.Purpose, &obj.Name, &obj.Size,
			&obj.Mime, &obj.Location, &obj.Status, &obj.CreatedAt, &s3UpID, &sessKeyNull)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if obj.Owner != userID {
		return nil, false, ErrForbidden
	}
	// Legacy sessions have no stored key — fall back to the row's sha key.
	// 旧会话无 s3_key 时退回按行内 sha 计算的 key。
	var legacySha []byte
	if err := s.pool.QueryRow(ctx, `SELECT sha256 FROM objects WHERE id=$1`, objID).Scan(&legacySha); err != nil {
		return nil, false, err
	}
	sessKey = s.s3.keyFrom(sessKeyNull, legacySha)
	key := sessKey

	finishTx := func() (err error) {
		return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
			head, err := s.s3.client.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(key),
			})
			if err != nil {
				return fmt.Errorf("s3 head: %w", err)
			}
			if aws.ToInt64(head.ContentLength) != obj.Size {
				return fmt.Errorf("size mismatch: declared %d got %d", obj.Size, aws.ToInt64(head.ContentLength))
			}
			return nil
		})
	}
	if err := finishTx(); err != nil {
		return nil, false, err
	}

	// Stream the assembled object once: hash is the truth, image dims ride
	// the same pass. / 组装后流式读一遍：哈希为真，图片尺寸同程嗅探。
	body, err := s.s3.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, false, fmt.Errorf("s3 get: %w", err)
	}
	h := sha256.New()
	tr := io.TeeReader(body.Body, h)
	var w, hh *int
	if strings.HasPrefix(obj.Mime, "image/") {
		w, hh = SniffDims(tr)
	}
	_, _ = io.Copy(io.Discard, tr)
	_ = body.Body.Close()
	obj.SHA256 = [32]byte(h.Sum(nil))
	obj.Width, obj.Height = w, hh
	obj.Status = "ready"

	var dedupID uuid.UUID
	hasDedup := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
			return err
		}
		// Same content already ready for this owner+conv → become a dedup.
		// 同属主+会话已有相同内容的 ready 对象 → 本次上传降级为去重。
		derr := tx.QueryRow(ctx,
			`SELECT id FROM objects WHERE sha256=$1 AND owner_user=$2
			   AND conv_id IS NOT DISTINCT FROM $3 AND status='ready' AND id<>$4 LIMIT 1`,
			obj.SHA256[:], obj.Owner, obj.ConvID, objID).Scan(&dedupID)
		if derr == nil {
			hasDedup = true
			if _, err := tx.Exec(ctx,
				`UPDATE objects SET status='deleted' WHERE id=$1`, objID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx,
				`UPDATE upload_sessions SET state='done' WHERE object_id=$1`, objID)
			return err
		}
		if !errors.Is(derr, pgx.ErrNoRows) {
			return derr
		}
		tag, err := tx.Exec(ctx,
			`UPDATE objects SET sha256=$2, status='ready', width=$3, height=$4,
			        dims_checked=true WHERE id=$1 AND status='pending'`,
			objID, obj.SHA256[:], w, hh)
		if err != nil {
			// Unique violation = a racing identical complete won — dedup.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				hasDedup = true
				if _, err := tx.Exec(ctx,
					`UPDATE objects SET status='deleted' WHERE id=$1`, objID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`UPDATE upload_sessions SET state='done' WHERE object_id=$1`, objID)
				return err
			}
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
			 VALUES ($1,0,$2,$3)`, objID, obj.Size, sessKey)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if hasDedup {
		_ = s.dropKeyIfOrphaned(ctx, key, "s3")
		var ex Object
		var exSha []byte
		if derr := s.pool.QueryRow(ctx,
			`SELECT id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status, created_at
			 FROM objects WHERE id=$1`, dedupID).
			Scan(&ex.ID, &ex.Owner, &ex.ConvID, &ex.Purpose, &ex.Name, &ex.Size,
				&exSha, &ex.Mime, &ex.Location, &ex.Status, &ex.CreatedAt); derr == nil {
			copy(ex.SHA256[:], exSha)
			return &ex, true, nil
		}
		return &obj, true, nil
	}
	return &obj, false, nil
}

// AbortExpiredUploads cancels open-but-expired multipart sessions: aborts
// the S3 upload (NoSuchUpload is a no-op success) and marks the pending
// object deleted. Fixes the stale-upload storage leak.
// 中止过期分片会话：中止 S3 上传（不存在视为成功）并把 pending 行标删。
func (s *Store) AbortExpiredUploads(ctx context.Context) (int, error) {
	if s.s3 == nil {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT u.id, u.object_id, u.s3_upload_id, u.s3_key, o.sha256
		 FROM upload_sessions u JOIN objects o ON o.id=u.object_id
		 WHERE u.state='open' AND u.expires_at < now()`)
	if err != nil {
		return 0, err
	}
	type sess struct {
		id, objID, upID, key string
		sha                  []byte
	}
	var list []sess
	for rows.Next() {
		var v sess
		var k *string
		if err := rows.Scan(&v.id, &v.objID, &v.upID, &k, &v.sha); err != nil {
			rows.Close()
			return 0, err
		}
		v.key = s.s3.keyFrom(k, v.sha)
		list = append(list, v)
	}
	rows.Close()
	n := 0
	for _, v := range list {
		oid, _ := uuid.Parse(v.objID)
		_, aerr := s.s3.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(v.key), UploadId: aws.String(v.upID),
		})
		var noSuch *s3types.NoSuchUpload
		if aerr != nil && !errors.As(aerr, &noSuch) {
			log.Printf("abort upload %s: %v", v.upID, aerr)
			continue
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE upload_sessions SET state='aborted' WHERE id=$1`, v.id); err == nil {
			_, _ = s.pool.Exec(ctx,
				`UPDATE objects SET status='deleted' WHERE id=$1 AND status='pending'`, oid)
			n++
		}
	}
	return n, nil
}

// keyFrom picks the stored session key or falls back to the sha key.
func (s *S3) keyFrom(sessKey *string, sha []byte) string {
	if sessKey != nil {
		return *sessKey
	}
	var a [32]byte
	copy(a[:], sha)
	return s.key(a)
}

// S3Enabled reports whether the store has an S3 backend.
func (s *Store) S3Enabled() bool { return s.s3 != nil }

// OpenS3 streams an s3-resident object — no access-time touch here;
// callers that count as user traffic use Open. / 读 S3 对象字节——
// 此处不更新访问时间；算用户访问的调用方走 Open。
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
	return out.Body, nil
}
