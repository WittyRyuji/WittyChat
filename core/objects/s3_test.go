// S3-path integration tests. Require a real S3-compatible endpoint AND the
// test DB. Skipped unless WITTYCHAT_TEST_S3_ENDPOINT (+ _BUCKET/_ACCESS_KEY/
// _SECRET_KEY, optional _REGION) and WITTYCHAT_TEST_DB_URL are set.
//
// S3 路径集成测试：需要真实 S3 端点与测试库；未设置环境变量时跳过。
// Each test uploads under a unique Prefix and removes it in cleanup.
package objects

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
)

// s3Env attaches an S3 backend pointed at a unique test prefix.
func s3Env(t *testing.T) (*Store, context.Context, func() uuid.UUID, *S3) {
	t.Helper()
	endpoint := os.Getenv("WITTYCHAT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("WITTYCHAT_TEST_S3_ENDPOINT not set")
	}
	s, ctx, mk := testEnv(t)
	var pref [4]byte
	_, _ = rand.Read(pref[:])
	cfg := &S3Config{
		Endpoint:  endpoint,
		Bucket:    os.Getenv("WITTYCHAT_TEST_S3_BUCKET"),
		Region:    envOr("WITTYCHAT_TEST_S3_REGION", "us-east-1"),
		AccessKey: os.Getenv("WITTYCHAT_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("WITTYCHAT_TEST_S3_SECRET_KEY"),
		Prefix:    fmt.Sprintf("test-%x/", pref[:]),
		Enabled:   true,
	}
	if cfg.Bucket == "" || cfg.AccessKey == "" {
		t.Skip("WITTYCHAT_TEST_S3_BUCKET / _ACCESS_KEY / _SECRET_KEY not set")
	}
	s3b := NewS3(cfg, s.dataDir)
	s.EnableS3(s3b)
	t.Cleanup(func() {
		ctx2, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var cont *string
		for {
			out, err := s3b.client.ListObjectsV2(ctx2, &s3.ListObjectsV2Input{
				Bucket: aws.String(cfg.Bucket), Prefix: aws.String(cfg.Prefix), ContinuationToken: cont,
			})
			if err != nil {
				return
			}
			var ids []s3types.ObjectIdentifier
			for _, o := range out.Contents {
				ids = append(ids, s3types.ObjectIdentifier{Key: o.Key})
			}
			if len(ids) > 0 {
				_, _ = s3b.client.DeleteObjects(ctx2, &s3.DeleteObjectsInput{
					Bucket: aws.String(cfg.Bucket),
					Delete: &s3types.Delete{Objects: ids},
				})
			}
			if out.NextContinuationToken == nil {
				return
			}
			cont = out.NextContinuationToken
		}
	})
	return s, ctx, mk, s3b
}

func mkConv(t *testing.T, s *Store, ctx context.Context) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO conversations (id, kind) VALUES ($1,'dm')`, id); err != nil {
		t.Fatalf("mkConv: %v", err)
	}
	return id
}

func headS3(t *testing.T, s3b *S3, key string) error {
	t.Helper()
	_, err := s3b.client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(s3b.cfg.Bucket), Key: aws.String(key),
	})
	return err
}

// putParts uploads parts to presigned URLs and collects etags.
func putParts(t *testing.T, data []byte, urls []string) []s3types.CompletedPart {
	t.Helper()
	if len(urls) == 0 {
		t.Fatal("putParts: no urls — unexpected dedup hit")
	}
	parts := make([]s3types.CompletedPart, 0, len(urls))
	span := int64(len(data)) / int64(len(urls))
	for i, u := range urls {
		start := int64(i) * span
		end := start + span
		if i == len(urls)-1 {
			end = int64(len(data))
		}
		req, err := http.NewRequest(http.MethodPut, u, bytes.NewReader(data[start:end]))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("part %d put: %v", i+1, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("part %d put: %d", i+1, resp.StatusCode)
		}
		et := resp.Header.Get("ETag")
		if et == "" {
			t.Fatalf("part %d: no ETag in response headers", i+1)
		}
		parts = append(parts, s3types.CompletedPart{
			PartNumber: aws.Int32(int32(i + 1)), ETag: aws.String(et),
		})
	}
	return parts
}

// uploadNow runs the whole begin→put→complete flow.
func uploadNow(t *testing.T, s *Store, ctx context.Context, s3b *S3,
	owner, conv uuid.UUID, name, mime string, data []byte, declared *[32]byte,
) (*Object, bool) {
	t.Helper()
	up, urls, err := s.BeginMultipart(ctx, owner, conv, "attachment", name, mime, int64(len(data)), declared)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	obj, dedup, err := s.CompleteMultipart(ctx, up.ObjectID, owner, putParts(t, data, urls))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	return obj, dedup
}

// TestS3MultipartRoundTrip: 17 MiB random → 3 parts → complete → Open bytes
// identical, ready, location s3, segment key = uploadKey(id).
func TestS3MultipartRoundTrip(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)

	data := make([]byte, 17<<20)
	_, _ = rand.Read(data)
	sha := sha256Of(data)

	up, urls, err := s.BeginMultipart(ctx, owner, conv, "attachment", "big.bin",
		"application/octet-stream", int64(len(data)), &sha)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if len(urls) != 3 {
		t.Fatalf("parts=%d want 3", len(urls))
	}
	obj, dedup, err := s.CompleteMultipart(ctx, up.ObjectID, owner, putParts(t, data, urls))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if dedup {
		t.Fatal("unexpected dedup for fresh bytes")
	}
	if obj.Status != "ready" || obj.Location != "s3" {
		t.Fatalf("status=%s location=%s", obj.Status, obj.Location)
	}
	var segKey string
	if err := s.pool.QueryRow(ctx,
		`SELECT storage_key FROM object_segments WHERE object_id=$1`, obj.ID).Scan(&segKey); err != nil {
		t.Fatal(err)
	}
	if segKey != s3b.uploadKey(obj.ID) {
		t.Fatalf("segment key %q want %q", segKey, s3b.uploadKey(obj.ID))
	}
	rc, err := s.Open(ctx, obj)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("open bytes: err=%v equal=%v", err, bytes.Equal(got, data))
	}
}

// TestS3MultipartNoDeclaredSHA: begin without a sha hint — stored sha is the
// server-computed hash; segment key = uploadKey(id).
func TestS3MultipartNoDeclaredSHA(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)
	data := []byte("no-declared-sha-" + uuid.Must(uuid.NewV7()).String())

	obj, dedup := uploadNow(t, s, ctx, s3b, owner, conv, "n.bin",
		"application/octet-stream", data, nil)
	if dedup {
		t.Fatal("unexpected dedup")
	}
	if obj.SHA256 != sha256Of(data) {
		t.Fatal("stored sha != server-computed hash")
	}
	if k := localKeyOf(t, s, ctx, obj.ID); k != s3b.uploadKey(obj.ID) {
		t.Fatalf("segment key %q want %q", k, s3b.uploadKey(obj.ID))
	}
}

// TestS3MultipartImageDims: single-part PNG completes with dims checked.
func TestS3MultipartImageDims(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)

	img := randImgBytes(t, "png", 320, 240)
	obj, _ := uploadNow(t, s, ctx, s3b, owner, conv, "img.png", "image/png", img, nil)
	if obj.Width == nil || *obj.Width != 320 || obj.Height == nil || *obj.Height != 240 {
		t.Fatalf("dims %v,%v want 320,240", obj.Width, obj.Height)
	}
	if _, _, checked := dimsOf(t, s, ctx, obj.ID); !checked {
		t.Fatal("dims_checked=false after complete")
	}
}

// TestS3DedupHintScoped (P1 regression): B's init with A's sha must NOT leak
// A's object id — scoped dedup means B just gets an upload of their own.
func TestS3DedupHintScoped(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	a, b := mk(), mk()
	data := []byte("dedup-scope-" + uuid.Must(uuid.NewV7()).String())
	sha := sha256Of(data)
	oA, _ := uploadNow(t, s, ctx, s3b, a, mkConv(t, s, ctx), "a.bin",
		"application/octet-stream", data, &sha)

	up, urls, err := s.BeginMultipart(ctx, b, mkConv(t, s, ctx), "attachment",
		"b.bin", "application/octet-stream", int64(len(data)), &sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) == 0 || up.ObjectID == oA.ID {
		t.Fatalf("P1 regression: B got A's object id / no urls (dedup leaked)")
	}
	// B completes with the same bytes — same sha, different owner+conv → own object.
	obj, dedup, err := s.CompleteMultipart(ctx, up.ObjectID, b, putParts(t, data, urls))
	if err != nil {
		t.Fatal(err)
	}
	if dedup || obj.ID == oA.ID {
		t.Fatalf("B's object must be its own row: dedup=%v id=%s A=%s", dedup, obj.ID, oA.ID)
	}
}

// TestS3DeclaredShaMismatch (P2 regression): attacker declares victim's sha,
// uploads junk → stored sha = sha(junk); after victim's local object with
// sha(X) migrates, attacker's Open still returns junk and the keys differ.
func TestS3DeclaredShaMismatch(t *testing.T) {
	s, ctx, mk, _ := s3Env(t)
	attacker, victim := mk(), mk()
	x := []byte("victim-secret-" + uuid.Must(uuid.NewV7()).String())
	shaX := sha256Of(x)
	junk := make([]byte, len(x))
	_, _ = rand.Read(junk)

	up, urls, err := s.BeginMultipart(ctx, attacker, mkConv(t, s, ctx),
		"attachment", "atk.bin", "application/octet-stream", int64(len(x)), &shaX)
	if err != nil {
		t.Fatal(err)
	}
	atkObj, _, err := s.CompleteMultipart(ctx, up.ObjectID, attacker, putParts(t, junk, urls))
	if err != nil {
		t.Fatal(err)
	}
	if atkObj.SHA256 != sha256Of(junk) {
		t.Fatal("stored sha != sha(junk) — declared sha must be ignored")
	}
	// Victim's local object with real X bytes → migrate to s3.
	oV, _, err := s.Put(ctx, victim, uuid.Nil, "attachment", "vic.bin",
		"application/octet-stream", bytes.NewReader(x), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET created_at=now()-interval '100 days',
		        last_accessed_at=now()-interval '100 days' WHERE id=$1`, oV.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateCold(ctx, 90*24*time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	rc, _ := s.Open(ctx, atkObj)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, junk) {
		t.Fatal("attacker's open must return the uploaded junk, not victim bytes")
	}
	oV2, _ := s.Peek(ctx, oV.ID)
	rc2, _ := s.Open(ctx, oV2)
	got2, _ := io.ReadAll(rc2)
	_ = rc2.Close()
	if !bytes.Equal(got2, x) {
		t.Fatal("victim's open must return victim bytes")
	}
	if localKeyOf(t, s, ctx, atkObj.ID) == localKeyOf(t, s, ctx, oV.ID) {
		t.Fatal("attacker and victim share a storage key — collision not fixed")
	}
}

// TestS3DedupAtComplete: same owner+conv, identical bytes twice — the second
// complete returns (first id, true); its upload key is deleted, row deleted.
func TestS3DedupAtComplete(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)
	data := []byte("dup-at-complete-" + uuid.Must(uuid.NewV7()).String())

	first, _ := uploadNow(t, s, ctx, s3b, owner, conv, "a.bin",
		"application/octet-stream", data, nil)

	// Second upload WITHOUT the hint (hint would have dedup'd at init).
	up2, urls2, err := s.BeginMultipart(ctx, owner, conv, "attachment", "a2.bin",
		"application/octet-stream", int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	secondKey := s3b.uploadKey(up2.ObjectID)
	second, dedup, err := s.CompleteMultipart(ctx, up2.ObjectID, owner, putParts(t, data, urls2))
	if err != nil {
		t.Fatal(err)
	}
	if !dedup || second.ID != first.ID {
		t.Fatalf("dedup=%v id=%s want true/%s", dedup, second.ID, first.ID)
	}
	var st string
	_ = s.pool.QueryRow(ctx, `SELECT status FROM objects WHERE id=$1`, up2.ObjectID).Scan(&st)
	if st != "deleted" {
		t.Fatalf("dup row status=%s want deleted", st)
	}
	if err := headS3(t, s3b, secondKey); err == nil {
		t.Fatal("dup object's upload key not removed")
	}
	if err := headS3(t, s3b, s3b.uploadKey(first.ID)); err != nil {
		t.Fatal("first object's key must survive")
	}
}

// TestS3AbortExpiredUploads: expired open session → S3 upload aborted,
// session aborted, pending row deleted.
func TestS3AbortExpiredUploads(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)
	data := []byte("stale-upload-" + uuid.Must(uuid.NewV7()).String())

	up, _, err := s.BeginMultipart(ctx, owner, conv, "attachment", "s.bin",
		"application/octet-stream", int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	key := s3b.uploadKey(up.ObjectID)
	if _, err := s.pool.Exec(ctx,
		`UPDATE upload_sessions SET expires_at=now()-interval '1 hour' WHERE object_id=$1`, up.ObjectID); err != nil {
		t.Fatal(err)
	}
	// n≥1 — the shared test DB may hold other expired sessions.
	n, err := s.AbortExpiredUploads(ctx)
	if err != nil || n < 1 {
		t.Fatalf("abort: n=%d err=%v", n, err)
	}
	var st, sess string
	_ = s.pool.QueryRow(ctx, `SELECT status FROM objects WHERE id=$1`, up.ObjectID).Scan(&st)
	_ = s.pool.QueryRow(ctx, `SELECT state FROM upload_sessions WHERE object_id=$1`, up.ObjectID).Scan(&sess)
	if st != "deleted" || sess != "aborted" {
		t.Fatalf("status=%s session=%s want deleted/aborted", st, sess)
	}
	lmu, err := s3b.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(s3b.cfg.Bucket), Prefix: aws.String(key),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lmu.Uploads) != 0 {
		t.Fatalf("%d s3 uploads still open under %s", len(lmu.Uploads), key)
	}
}

// TestS3MultipartConvNil: conv-less multipart works; conv_id is NULL.
func TestS3MultipartConvNil(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner := mk()
	data := []byte("conv-nil-" + uuid.Must(uuid.NewV7()).String())
	obj, _ := uploadNow(t, s, ctx, s3b, owner, uuid.Nil, "n.bin",
		"application/octet-stream", data, nil)
	var cv *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT conv_id FROM objects WHERE id=$1`, obj.ID).Scan(&cv); err != nil {
		t.Fatal(err)
	}
	if cv != nil {
		t.Fatalf("conv_id=%v want NULL", cv)
	}
}

// TestS3MigrateColdShared: two objects same bytes, different owners — local
// file survives while referenced, removed once the last migrates.
func TestS3MigrateColdShared(t *testing.T) {
	s, ctx, mk, _ := s3Env(t)
	u1, u2 := mk(), mk()
	data := []byte("cold-shared-" + uuid.Must(uuid.NewV7()).String())

	o1, _, err := s.Put(ctx, u1, uuid.Nil, "attachment", "a.bin",
		"application/octet-stream", bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	o2, _, err := s.Put(ctx, u2, uuid.Nil, "attachment", "b.bin",
		"application/octet-stream", bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if o1.ID == o2.ID {
		t.Fatal("same object id — dedup must not fire across owners")
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET created_at=now()-interval '100 days',
		        last_accessed_at=now()-interval '100 days' WHERE id=$1`, o1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateCold(ctx, 90*24*time.Hour, 10); err != nil {
		t.Fatalf("migrate1: %v", err)
	}
	o1x, _ := s.Peek(ctx, o1.ID)
	local := filepath.Join(s.dataDir, filepath.FromSlash(localKeyOf(t, s, ctx, o2.ID)))
	if o1x.Location != "s3" {
		t.Fatalf("o1 location=%s want s3", o1x.Location)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("local file must survive while still referenced: %v", err)
	}
	rc, _ := s.Open(ctx, o1x)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("s3 bytes differ after migrate")
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET created_at=now()-interval '100 days',
		        last_accessed_at=now()-interval '100 days' WHERE id=$1`, o2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateCold(ctx, 90*24*time.Hour, 10); err != nil {
		t.Fatalf("migrate2: %v", err)
	}
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatalf("local file must be gone once unreferenced (stat err=%v)", err)
	}
}

// TestS3ReplaceBytesVersions: ReplaceBytes on an s3 object flips to local;
// the version row records the s3 key; purge keeps it while referenced,
// removal drops it.
func TestS3ReplaceBytesVersions(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)

	orig := []byte("orig-s3-" + uuid.Must(uuid.NewV7()).String())
	obj, _ := uploadNow(t, s, ctx, s3b, owner, conv, "f.bin",
		"application/octet-stream", orig, nil)
	s3key := localKeyOf(t, s, ctx, obj.ID)

	owner2 := mk()
	objB, _, err := s.Put(ctx, owner2, uuid.Nil, "attachment", "g.bin",
		"application/octet-stream", bytes.NewReader(orig), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET created_at=now()-interval '100 days',
		        last_accessed_at=now()-interval '100 days' WHERE id=$1`, objB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MigrateCold(ctx, 90*24*time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	// objB's cold-migrated key is the sha key — different from obj's upload
	// key (content-addressed vs per-upload) — so to test key retention we
	// point objB's segment at obj's s3 key instead.
	if _, err := s.pool.Exec(ctx,
		`UPDATE object_segments SET storage_key=$2 WHERE object_id=$1`, objB.ID, s3key); err != nil {
		t.Fatal(err)
	}

	newBytes := []byte("replacement-" + uuid.Must(uuid.NewV7()).String())
	obj2, err := s.ReplaceBytes(ctx, obj.ID, bytes.NewReader(newBytes),
		"application/octet-stream", 1<<20, &owner)
	if err != nil {
		t.Fatal(err)
	}
	if obj2.Location != "local" {
		t.Fatalf("location after replace: %s want local", obj2.Location)
	}
	var vkey string
	if err := s.pool.QueryRow(ctx,
		`SELECT segments->0->>'key' FROM object_versions WHERE object_id=$1 ORDER BY version LIMIT 1`,
		obj.ID).Scan(&vkey); err != nil {
		t.Fatalf("version row: %v", err)
	}
	if vkey != s3key {
		t.Fatalf("version key %q want %q", vkey, s3key)
	}
	if _, err := s.PurgeOldVersions(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := headS3(t, s3b, s3key); err != nil {
		t.Fatalf("s3 key deleted while still referenced: %v", err)
	}
	objB2, _ := s.Peek(ctx, objB.ID)
	if _, err := s.pool.Exec(ctx, `UPDATE objects SET status='deleted' WHERE id=$1`, objB.ID); err != nil {
		t.Fatal(err)
	}
	s.RemoveFiles(ctx, objB2)
	if err := headS3(t, s3b, s3key); err == nil {
		t.Fatal("s3 key still present after last reference removed")
	}
}

// TestS3OrphanGC: backdated ready-unreferenced s3 object → deleted + S3
// object gone; backdated pending multipart → deleted.
func TestS3OrphanGC(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)

	ready := []byte("orphan-ready-" + uuid.Must(uuid.NewV7()).String())
	oR, _ := uploadNow(t, s, ctx, s3b, owner, conv, "r.bin",
		"application/octet-stream", ready, nil)
	keyR := localKeyOf(t, s, ctx, oR.ID)

	pend := []byte("orphan-pending-" + uuid.Must(uuid.NewV7()).String())
	upP, _, err := s.BeginMultipart(ctx, owner, conv, "attachment", "p.bin",
		"application/octet-stream", int64(len(pend)), nil)
	if err != nil {
		t.Fatal(err)
	}
	keyP := s3b.uploadKey(upP.ObjectID)

	for _, id := range []uuid.UUID{oR.ID, upP.ObjectID} {
		if _, err := s.pool.Exec(ctx,
			`UPDATE objects SET created_at=now()-interval '10 days' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.OrphanGC(ctx); err != nil {
		t.Fatal(err)
	}
	var stR, stP string
	_ = s.pool.QueryRow(ctx, `SELECT status FROM objects WHERE id=$1`, oR.ID).Scan(&stR)
	_ = s.pool.QueryRow(ctx, `SELECT status FROM objects WHERE id=$1`, upP.ObjectID).Scan(&stP)
	if stR != "deleted" || stP != "deleted" {
		t.Fatalf("statuses ready=%s pending=%s want deleted", stR, stP)
	}
	if err := headS3(t, s3b, keyR); err == nil {
		t.Fatal("ready orphan's s3 object still present")
	}
	lmu, lerr := s3b.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(s3b.cfg.Bucket), Prefix: aws.String(keyP),
	})
	if lerr == nil {
		t.Logf("pending GC: %d s3 multipart uploads still open under %s (OrphanGC alone does not abort — AbortExpiredUploads does)", len(lmu.Uploads), keyP)
	}
}

// TestS3VerifyHashNoTouch: VerifyHash returns true AND must not bump
// last_accessed_at — a weekly verify keeping everything hot would starve
// cold migration.
func TestS3VerifyHashNoTouch(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)
	data := []byte("verify-me-" + uuid.Must(uuid.NewV7()).String())
	obj, _ := uploadNow(t, s, ctx, s3b, owner, conv, "v.bin",
		"application/octet-stream", data, nil)

	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET last_accessed_at=now()-interval '100 days' WHERE id=$1`, obj.ID); err != nil {
		t.Fatal(err)
	}
	ok, err := s.VerifyHash(ctx, obj.ID)
	if err != nil || !ok {
		t.Fatalf("verifyhash: ok=%v err=%v", ok, err)
	}
	var touched bool
	if err := s.pool.QueryRow(ctx,
		`SELECT last_accessed_at > now()-interval '1 hour' FROM objects WHERE id=$1`, obj.ID).Scan(&touched); err != nil {
		t.Fatal(err)
	}
	if touched {
		t.Fatal("VerifyHash bumped last_accessed_at — cold migration will never trigger")
	}
}

// TestS3BackfillDims: an s3 image row with dims_checked=false gets dims via
// the sweep (openBytes → GetObject).
func TestS3BackfillDims(t *testing.T) {
	s, ctx, mk, s3b := s3Env(t)
	owner, conv := mk(), mkConv(t, s, ctx)
	jpg := randImgBytes(t, "jpeg", 64, 48)
	obj, _ := uploadNow(t, s, ctx, s3b, owner, conv, "b.jpg", "image/jpeg", jpg, nil)
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET width=NULL, height=NULL, dims_checked=false WHERE id=$1`, obj.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BackfillDims(ctx); err != nil {
		t.Fatal(err)
	}
	w, h, checked := dimsOf(t, s, ctx, obj.ID)
	if w == nil || *w != 64 || h == nil || *h != 48 || !checked {
		t.Fatalf("after backfill w=%v h=%v checked=%v want 64,48,true", w, h, checked)
	}
}

// TestPresignPublicEndpoint (pure, no network): presigned part URLs must
// point at PublicEndpoint when configured.
func TestPresignPublicEndpoint(t *testing.T) {
	s3b := NewS3(&S3Config{
		Endpoint:       "http://internal:3900",
		PublicEndpoint: "https://files.example.com",
		Bucket:         "b", Region: "us-east-1",
		AccessKey: "k", SecretKey: "s", Prefix: "p/",
		Enabled: true,
	}, t.TempDir())
	pre, err := s3b.presign.PresignUploadPart(context.Background(), &s3.UploadPartInput{
		Bucket:     aws.String("b"),
		Key:        aws.String("p/up/x"),
		UploadId:   aws.String("u"),
		PartNumber: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(pre.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "files.example.com" {
		t.Fatalf("presign host %q want files.example.com", u.Host)
	}
	// Without PublicEndpoint the internal endpoint is used.
	s3b2 := NewS3(&S3Config{
		Endpoint: "http://internal:3900",
		Bucket:   "b", Region: "us-east-1",
		AccessKey: "k", SecretKey: "s", Prefix: "p/",
		Enabled: true,
	}, t.TempDir())
	pre2, err := s3b2.presign.PresignUploadPart(context.Background(), &s3.UploadPartInput{
		Bucket:     aws.String("b"),
		Key:        aws.String("p/up/x"),
		UploadId:   aws.String("u"),
		PartNumber: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	u2, _ := url.Parse(pre2.URL)
	if u2.Host != "internal:3900" {
		t.Fatalf("presign host %q want internal:3900", u2.Host)
	}
}

func localKeyOf(t *testing.T, s *Store, ctx context.Context, id uuid.UUID) string {
	t.Helper()
	var k string
	if err := s.pool.QueryRow(ctx,
		`SELECT storage_key FROM object_segments WHERE object_id=$1 ORDER BY seg_index LIMIT 1`,
		id).Scan(&k); err != nil {
		t.Fatal(err)
	}
	return k
}

// randImgBytes encodes a w×h image with a random-tinted gradient so the
// content (and sha) is unique per run — deterministic encoders would dedup
// against objects left in the shared test DB.
func randImgBytes(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	var rnd [3]byte
	_, _ = rand.Read(rnd[:])
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, color.RGBA{uint8((x + int(rnd[0])) & 0xff), uint8((y + int(rnd[1])) & 0xff), rnd[2], 255}) //nolint:gosec // test fill — wrapping cast is intended
		}
	}
	var buf bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&buf, im, &jpeg.Options{Quality: 85})
	} else {
		err = png.Encode(&buf, im)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Of(b []byte) [32]byte {
	return sha256.Sum256(b)
}
