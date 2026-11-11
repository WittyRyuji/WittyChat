package mailimport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
	"wittychat/core/mail"
	"wittychat/core/objects"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testEnv: real test DB + objects store under a temp dir + mail store +
// a fixed secret key (tests assert seal/open, not rotation).
func testEnv(t *testing.T) (*Store, context.Context, *pgxpool.Pool, func() uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("WITTYCHAT_TEST_DB_URL")
	if dsn == "" {
		t.Skip("WITTYCHAT_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	obj, err := objects.NewStore(pool, t.TempDir())
	if err != nil {
		t.Fatalf("objects: %v", err)
	}
	ms := mail.NewStore(pool, "wittyloong.com")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	imp := New(pool, obj, ms, key)
	mk := func() uuid.UUID {
		t.Helper()
		var id uuid.UUID
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		em := fmt.Sprintf("imp-%x@wittyloong.com", b)
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (id,email,display_name,status,is_admin) VALUES ($1,$2,$3,'active',false) RETURNING id`,
			uuid.Must(uuid.NewV7()), em, em).Scan(&id); err != nil {
			t.Fatalf("mkUser: %v", err)
		}
		if err := ms.EnsureFolders(ctx, id); err != nil {
			t.Fatalf("folders: %v", err)
		}
		return id
	}
	return imp, ctx, pool, mk
}

type testLit struct {
	r *strings.Reader
	n int64
}

func (l *testLit) Read(b []byte) (int, error) { return l.r.Read(b) }
func (l *testLit) Size() int64                { return l.n }

type appMsg struct {
	raw   string
	flags []imap.Flag
	when  time.Time
}

// memServer spins up an in-process IMAP server with the fixture mailboxes.
func memServer(t *testing.T, appends map[string][]appMsg) string {
	addr, _ := memServerUser(t, appends)
	return addr
}

// memServerUser returns (addr, user) so tests can Append more mail mid-run.
func memServerUser(t *testing.T, appends map[string][]appMsg) (string, *imapmemserver.User) {
	t.Helper()
	user := imapmemserver.NewUser("src@old.test", "pw")
	for name, opts := range map[string]*imap.CreateOptions{
		"INBOX":   nil,
		"已发送":     {SpecialUse: []imap.MailboxAttr{"\\Sent"}},
		"项目/2024": nil,
		"Trash":   nil,
	} {
		if err := user.Create(name, opts); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	for mbox, msgs := range appends {
		for _, m := range msgs {
			if _, err := user.Append(mbox,
				&testLit{r: strings.NewReader(m.raw), n: int64(len(m.raw))},
				&imap.AppendOptions{Flags: m.flags, Time: m.when}); err != nil {
				t.Fatalf("append %s: %v", mbox, err)
			}
		}
	}
	mem := imapmemserver.New()
	mem.AddUser(user)
	imapSrv := imapserver.New(&imapserver.Options{
		InsecureAuth: true, // plaintext LOGIN for tests / 测试允许明文登录
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = imapSrv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), user
}

var (
	fxPlain = "From: Bob <bob@x.cn>\r\nTo: alice@wittyloong.com\r\nSubject: hello\r\n" +
		"Date: Mon, 03 Feb 2025 09:00:00 +0800\r\nMessage-ID: <p1@x>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nplain body 你好\r\n"
	fxGBK = "From: =?GBK?B?1cXI/Q==?= <zhang@example.cn>\r\nTo: alice@wittyloong.com\r\n" +
		"Subject: =?GBK?B?u+HS6Q==?=\r\n" +
		"Content-Type: text/plain; charset=GBK\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("\xC4\xE3\xBA\xC3")) + "\r\n"
	fxNested = "From: x@y.z\r\nTo: alice@wittyloong.com\r\nSubject: nested\r\nMessage-ID: <n1@x>\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=M1\r\n\r\n" +
		"--M1\r\nContent-Type: multipart/related; boundary=M2\r\n\r\n" +
		"--M2\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<img src=\"cid:logo@x\"> hi\r\n" +
		"--M2\r\nContent-Type: image/png; name=\"l.png\"\r\nContent-ID: <logo@x>\r\n" +
		"Content-Transfer-Encoding: base64\r\nContent-Disposition: inline; filename=\"l.png\"\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'}) + "\r\n" +
		"--M2--\r\n" +
		"--M1\r\nContent-Type: application/pdf; name*=UTF-8''%E6%8A%A5%E5%91%8A.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename*=UTF-8''%E6%8A%A5%E5%91%8A.pdf\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("%PDF fake")) + "\r\n--M1--\r\n"
	fxSeen = "From: c@d.e\r\nTo: alice@wittyloong.com\r\nSubject: flagged\r\n" +
		"Date: Tue, 04 Feb 2025 10:00:00 +0800\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nseen+flagged\r\n"
)

// seedAccount: job + csv account + start, returns (jobID, acctID, targetUser).
func seedAccount(t *testing.T, imp *Store, ctx context.Context, pool *pgxpool.Pool,
	mk func() uuid.UUID, addr string, csvText string,
) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	target := mk()
	host, port := "127.0.0.1", 0
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
		_, _ = fmt.Sscanf(addr[i+1:], "%d", &port)
	}
	admin := mk()
	jobID, err := imp.CreateJob(ctx, JobParams{
		Name: "t", Host: host, Port: port, TLSMode: "plain", AddAlias: true, CreatedBy: admin,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	added, _, errs := imp.AddAccountsCSV(ctx, jobID, csvText+","+emailOf(ctx, pool, target))
	if len(errs) > 0 || added != 1 {
		t.Fatalf("csv: added=%d errs=%v", added, errs)
	}
	var acctID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM mail_import_accounts WHERE job_id=$1`, jobID).Scan(&acctID); err != nil {
		t.Fatal(err)
	}
	if err := imp.StartJob(ctx, jobID); err != nil {
		t.Fatalf("start: %v", err)
	}
	return jobID, acctID, target
}

func emailOf(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) string {
	var em string
	_ = pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, id).Scan(&em)
	return em
}

func jobOf(ctx context.Context, pool *pgxpool.Pool, acctID uuid.UUID) uuid.UUID {
	var j uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT job_id FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&j)
	return j
}

func runAll(t *testing.T, imp *Store, ctx context.Context, acctID uuid.UUID) {
	t.Helper()
	for i := 0; i < 50; i++ {
		more, err := imp.RunAccount(ctx, acctID, 30*time.Second)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !more {
			return
		}
	}
	t.Fatal("RunAccount never finished")
}

func TestImportEndToEnd(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	addr := memServer(t, map[string][]appMsg{
		"INBOX": {
			{fxPlain, nil, time.Date(2025, 2, 3, 1, 0, 0, 0, time.UTC)},
			{fxGBK, nil, time.Date(2025, 2, 3, 2, 0, 0, 0, time.UTC)},
			{fxNested, nil, time.Date(2025, 2, 3, 3, 0, 0, 0, time.UTC)},
			{fxSeen, []imap.Flag{"\\Seen", "\\Flagged"}, time.Date(2025, 2, 4, 2, 0, 0, 0, time.UTC)},
		},
		"已发送":     {{fxPlain, nil, time.Date(2025, 2, 5, 1, 0, 0, 0, time.UTC)}},
		"项目/2024": {{fxPlain, nil, time.Date(2025, 2, 6, 1, 0, 0, 0, time.UTC)}},
	})
	jobID, acctID, target := seedAccount(t, imp, ctx, pool, mk, addr, "src@old.test,pw")
	runAll(t, imp, ctx, acctID)

	var st, jst string
	var done, total, skipped int
	if err := pool.QueryRow(ctx,
		`SELECT status,msgs_total,msgs_done,msgs_skipped FROM mail_import_accounts WHERE id=$1`, acctID).
		Scan(&st, &total, &done, &skipped); err != nil {
		t.Fatal(err)
	}
	if st != "done" || total != 6 || done != 6 || skipped != 0 {
		t.Fatalf("acct: st=%s total=%d done=%d skipped=%d", st, total, done, skipped)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM mail_import_jobs WHERE id=$1`, jobID).Scan(&jst); err != nil {
		t.Fatal(err)
	}
	if jst != "done" {
		t.Fatalf("job: %s", jst)
	}
	var sentDir, sentName, sentKind string
	if err := pool.QueryRow(ctx,
		`SELECT m.direction, f2.name, f2.kind FROM mail_messages m JOIN mail_folders f2 ON f2.id=m.folder_id
		 WHERE m.owner_user_id=$1 AND m.import_key IS NOT NULL AND f2.kind='sent'`, target).
		Scan(&sentDir, &sentName, &sentKind); err != nil {
		t.Fatalf("sent msg: %v", err)
	}
	if sentDir != "out" {
		t.Fatalf("sent dir: %s", sentDir)
	}
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_messages m JOIN mail_folders f ON f.id=m.folder_id
		 WHERE m.owner_user_id=$1 AND f.kind='custom' AND f.name='项目/2024'`, target).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("custom folder msgs: %d", cnt)
	}
	var isRead, isStarred bool
	var sentAt, recAt time.Time
	var ik *string
	var rawOID *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT is_read,is_starred,sent_at,received_at,import_key,raw_object_id
		 FROM mail_messages WHERE owner_user_id=$1 AND subject='flagged'`, target).
		Scan(&isRead, &isStarred, &sentAt, &recAt, &ik, &rawOID); err != nil {
		t.Fatal(err)
	}
	if !isRead || !isStarred || ik == nil || *ik == "" {
		t.Fatalf("flags/key: read=%v star=%v ik=%v", isRead, isStarred, ik)
	}
	if !sentAt.Equal(time.Date(2025, 2, 4, 10, 0, 0, 0, time.FixedZone("cst", 8*3600))) {
		t.Fatalf("sent_at %v want header Date", sentAt)
	}
	if !recAt.Equal(time.Date(2025, 2, 4, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("received_at %v want INTERNALDATE", recAt)
	}
	if rawOID == nil {
		t.Fatal("no raw object")
	}
	var purpose, mime string
	if err := pool.QueryRow(ctx, `SELECT purpose,mime FROM objects WHERE id=$1`, *rawOID).Scan(&purpose, &mime); err != nil {
		t.Fatal(err)
	}
	if purpose != "mail_raw" || mime != "message/rfc822" {
		t.Fatalf("raw: %s %s", purpose, mime)
	}
	var cid *string
	var inl bool
	if err := pool.QueryRow(ctx,
		`SELECT a.content_id,a.inline FROM mail_attachments a JOIN mail_messages m ON m.id=a.message_id
		 WHERE m.owner_user_id=$1 AND m.subject='nested' AND a.name='l.png'`, target).Scan(&cid, &inl); err != nil {
		t.Fatal(err)
	}
	if cid == nil || *cid != "logo@x" || !inl {
		t.Fatalf("att: cid=%v inl=%v", cid, inl)
	}
	var body string
	if err := pool.QueryRow(ctx,
		`SELECT text_body FROM mail_messages WHERE owner_user_id=$1 AND subject='会议'`, target).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "你好") {
		t.Fatalf("gbk body: %q", body)
	}
	var note string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(note,'') FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&note)
	if note != "来源域名非本地域，未添加别名" {
		t.Fatalf("note: %q", note)
	}
}

func TestImportIncremental(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	addr, user := memServerUser(t, map[string][]appMsg{
		"INBOX": {{fxPlain, nil, time.Now().Add(-time.Hour)}},
	})
	_, acctID, target := seedAccount(t, imp, ctx, pool, mk, addr, "src@old.test,pw")
	runAll(t, imp, ctx, acctID)
	var n1 int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_messages WHERE owner_user_id=$1 AND import_key IS NOT NULL`, target).Scan(&n1)
	// append one new message through the same user → rerun imports exactly 1
	newMsg := "From: z@w" + "\r\n" + "To: a@b" + "\r\n" +
		"Subject: new1" + "\r\n" + "Message-ID: <new1@x>" + "\r\n\r\n" + "fresh" + "\r\n"
	if _, err := user.Append("INBOX",
		&testLit{r: strings.NewReader(newMsg), n: int64(len(newMsg))},
		&imap.AppendOptions{}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := imp.StartJob(ctx, jobOf(ctx, pool, acctID)); err != nil {
		t.Fatal(err)
	}
	runAll(t, imp, ctx, acctID)
	var n2, tot, done int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_messages WHERE owner_user_id=$1 AND import_key IS NOT NULL`, target).Scan(&n2)
	_ = pool.QueryRow(ctx, `SELECT msgs_total,msgs_done FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&tot, &done)
	if n1 != 1 || n2 != 2 || done != 2 || tot != 2 {
		t.Fatalf("incremental: msgs=%d→%d tot=%d done=%d", n1, n2, tot, done)
	}
}

func TestImportUIDValidityReset(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	addr := memServer(t, map[string][]appMsg{"INBOX": {{fxPlain, nil, time.Now()}}})
	_, acctID, target := seedAccount(t, imp, ctx, pool, mk, addr, "src@old.test,pw")
	runAll(t, imp, ctx, acctID)
	// simulate a UIDVALIDITY change: stored value ≠ server → last_uid reset,
	// rescan finds the same messages → dedup via import_key/msgid, 0 new.
	if _, err := pool.Exec(ctx,
		`UPDATE mail_import_folders SET uidvalidity=uidvalidity+999 WHERE account_id=$1`, acctID); err != nil {
		t.Fatal(err)
	}
	if err := imp.StartJob(ctx, jobOf(ctx, pool, acctID)); err != nil {
		t.Fatal(err)
	}
	runAll(t, imp, ctx, acctID)
	var n, sk int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_messages WHERE owner_user_id=$1 AND import_key IS NOT NULL`, target).Scan(&n)
	_ = pool.QueryRow(ctx, `SELECT msgs_skipped FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&sk)
	if n != 1 || sk == 0 {
		t.Fatalf("uidv reset: msgs=%d skipped=%d", n, sk)
	}
}

func TestImportChunked(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	var msgs []appMsg
	for i := 0; i < 30; i++ {
		msgs = append(msgs, appMsg{fmt.Sprintf("From: x@y\r\nTo: a@b\r\nSubject: m%d\r\n\r\nbody %d", i, i), nil, time.Now()})
	}
	addr := memServer(t, map[string][]appMsg{"INBOX": msgs})
	_, acctID, target := seedAccount(t, imp, ctx, pool, mk, addr, "src@old.test,pw")
	more, err := imp.RunAccount(ctx, acctID, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60 && more; i++ {
		more, err = imp.RunAccount(ctx, acctID, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
	}
	var n, tot, done int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_messages WHERE owner_user_id=$1 AND import_key IS NOT NULL`, target).Scan(&n)
	_ = pool.QueryRow(ctx, `SELECT msgs_total,msgs_done FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&tot, &done)
	if n != 30 || tot != 30 || done != 30 {
		t.Fatalf("chunked: msgs=%d total=%d done=%d want 30/30/30", n, tot, done)
	}
}

func TestImportBadPassword(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	addr := memServer(t, map[string][]appMsg{"INBOX": {{fxPlain, nil, time.Now()}}})
	_, acctID, _ := seedAccount(t, imp, ctx, pool, mk, addr, "src@old.test,WRONGPW")
	more, err := imp.RunAccount(ctx, acctID, 30*time.Second)
	if err != nil {
		t.Fatalf("auth fail should not retry: %v", err)
	}
	var st, le string
	_ = pool.QueryRow(ctx,
		`SELECT status,COALESCE(last_error,'') FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&st, &le)
	if st != "error" || !strings.Contains(le, "专用密码") {
		t.Fatalf("badpw: st=%s err=%q", st, le)
	}
	_ = more
}

// TestTestAccountsAll: nil ids (what the admin UI sends for "测试连接") must
// test every account — a nil slice binds SQL NULL, which once matched none.
// nil 即全部：UI 不传 account_ids，曾因 NULL 一个都不测。
func TestTestAccountsAll(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	addr := memServer(t, map[string][]appMsg{"INBOX": {{fxPlain, nil, time.Now()}}})
	jobID, _, _ := seedAccount(t, imp, ctx, pool, mk, addr, "src@old.test,pw")
	if err := imp.PauseJob(ctx, jobID); err != nil { // uploads are refused while running
		t.Fatal(err)
	}
	if _, _, errs := imp.AddAccountsCSV(ctx, jobID,
		"bad@old.test,WRONG,"+emailOf(ctx, pool, mk())+"\n"); len(errs) != 0 {
		t.Fatal(errs)
	}
	res, err := imp.TestAccounts(ctx, jobID, nil)
	if err != nil || len(res) != 2 {
		t.Fatalf("test all: n=%d err=%v", len(res), err)
	}
	okN, badN := 0, 0
	for _, r := range res {
		if r.OK {
			okN++
		} else if strings.Contains(r.Error, "专用密码") {
			badN++
		}
	}
	if okN != 1 || badN != 1 {
		t.Fatalf("test all: ok=%d bad=%d res=%+v", okN, badN, res)
	}
}

func TestPlainNonLoopbackRejected(t *testing.T) {
	imp, ctx, _, mk := testEnv(t)
	_, err := imp.CreateJob(ctx, JobParams{
		Name: "x", Host: "imap.feishu.cn", Port: 143, TLSMode: "plain", CreatedBy: mk(),
	})
	if err == nil {
		t.Fatal("plain remote accepted")
	}
}

func TestSecretsSealedAndPurged(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	addr := memServer(t, map[string][]appMsg{"INBOX": {}})
	jobID, acctID, _ := seedAccount(t, imp, ctx, pool, mk, addr, "src@old.test,pw")
	var sealed []byte
	if err := pool.QueryRow(ctx,
		`SELECT sealed_secret FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if sealed == nil || strings.Contains(string(sealed), "pw") || string(sealed) == "pw" {
		t.Fatal("plaintext leaked into sealed_secret")
	}
	if err := imp.FinishJob(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	var null *bool
	var purged bool
	_ = pool.QueryRow(ctx,
		`SELECT sealed_secret IS NULL,secrets_purged FROM mail_import_accounts a JOIN mail_import_jobs j ON j.id=a.job_id WHERE a.id=$1`,
		acctID).Scan(&null, &purged)
	if null == nil || !*null || !purged {
		t.Fatalf("purge: null=%v purged=%v", null, purged)
	}
	if err := imp.StartJob(ctx, jobID); err == nil {
		t.Fatal("start after purge accepted")
	}
}

func TestAddAccountsCSVErrors(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	target := mk()
	jobID, err := imp.CreateJob(ctx, JobParams{
		Name: "t", Host: "127.0.0.1", Port: 1, TLSMode: "plain", CreatedBy: mk(),
	})
	if err != nil {
		t.Fatal(err)
	}
	csvText := "source_email,password,target_email\n" +
		"a@x,pw1,ghost@nowhere\n" +
		"b@x,pw2," + emailOf(ctx, pool, target) + "\n"
	added, _, errs := imp.AddAccountsCSV(ctx, jobID, csvText)
	if added != 1 || len(errs) != 1 || !strings.Contains(errs[0], "目标用户不存在") {
		t.Fatalf("csv: added=%d errs=%v (want the friendly unknown-target error, not an FK error)", added, errs)
	}
	csvText2 := "b@x,pw2b," + emailOf(ctx, pool, target) + "\n"
	added, updated, errs2 := imp.AddAccountsCSV(ctx, jobID, csvText2)
	if added != 0 || updated != 1 || len(errs2) != 0 {
		t.Fatalf("re-upload: added=%d updated=%d errs=%v", added, updated, errs2)
	}
}

func TestRequeueStale(t *testing.T) {
	_, ctx, pool, _ := testEnv(t)
	obj, _ := objects.NewStore(pool, t.TempDir())
	var staleID, freshID int64
	_ = pool.QueryRow(ctx,
		`INSERT INTO bg_tasks (kind,status,locked_by,locked_at) VALUES ('x','running','w',now()-interval '2 hours') RETURNING id`).Scan(&staleID)
	_ = pool.QueryRow(ctx,
		`INSERT INTO bg_tasks (kind,status,locked_by,locked_at) VALUES ('x','running','w',now()) RETURNING id`).Scan(&freshID)
	n, err := obj.RequeueStale(ctx, 60*time.Minute)
	// >=1: the shared test DB may hold other tests' aged claimed tasks; the
	// per-row checks below are the real assertion. / 共享测试库可能有别的陈旧任务。
	if err != nil || n < 1 {
		t.Fatalf("requeue: n=%d err=%v", n, err)
	}
	var st1, st2 string
	_ = pool.QueryRow(ctx, `SELECT status FROM bg_tasks WHERE id=$1`, staleID).Scan(&st1)
	_ = pool.QueryRow(ctx, `SELECT status FROM bg_tasks WHERE id=$1`, freshID).Scan(&st2)
	if st1 != "queued" || st2 != "running" {
		t.Fatalf("stale=%s fresh=%s", st1, st2)
	}
}

func TestOrphanGCRespectsRawMail(t *testing.T) {
	_, ctx, pool, mk := testEnv(t)
	target := mk()
	obj, _ := objects.NewStore(pool, t.TempDir())
	data := []byte("raw eml " + uuid.Must(uuid.NewV7()).String()[:8])
	o, _, err := obj.Put(ctx, target, uuid.Nil, "mail", "x.eml", "message/rfc822",
		strings.NewReader(string(data)), int64(len(data))+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE objects SET created_at=now()-interval '8 days', last_accessed_at=now()-interval '8 days' WHERE id=$1`, o.ID); err != nil {
		t.Fatal(err)
	}
	ms := mail.NewStore(pool, "wittyloong.com")
	p := &mail.Parsed{From: "x@y", To: []string{emailOf(ctx, pool, target)}, Subject: "s", Text: "t"}
	if _, err := ms.DeliverParsed(ctx, target, "inbox", nil, p, mail.DeliverOpts{
		RawObjectID: &o.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := obj.OrphanGC(ctx); err != nil {
		t.Fatal(err)
	}
	var alive bool
	_ = pool.QueryRow(ctx, `SELECT status='ready' FROM objects WHERE id=$1`, o.ID).Scan(&alive)
	if !alive {
		t.Fatal("raw mail object GC'd")
	}
}

// TestImportFailureRetry: a transient per-message failure is journaled,
// retried next pass, and never loses the mail.
func TestImportFailureRetry(t *testing.T) {
	imp, ctx, pool, mk := testEnv(t)
	addr := memServer(t, map[string][]appMsg{
		"INBOX": {
			{fxPlain, nil, time.Now()},
			{fxSeen, nil, time.Now()},
		},
	})
	_, acctID, target := seedAccount(t, imp, ctx, pool, mk, addr, "src@old.test,pw")
	failOnce := true
	testFailUID = func(folder string, uid imap.UID) error {
		if failOnce && folder == "INBOX" && uid == 1 {
			return errors.New("injected failure")
		}
		return nil
	}
	runAll(t, imp, ctx, acctID)
	var nRows, failed int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_import_failures WHERE account_id=$1`, acctID).Scan(&nRows)
	_ = pool.QueryRow(ctx,
		`SELECT msgs_failed FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&failed)
	if nRows != 1 || failed != 1 {
		t.Fatalf("journal: rows=%d failed=%d", nRows, failed)
	}
	var st string
	var n int
	_ = pool.QueryRow(ctx, `SELECT status FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&st)
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_messages WHERE owner_user_id=$1 AND import_key IS NOT NULL`, target).Scan(&n)
	if st != "done" || n != 1 {
		t.Fatalf("after fail: st=%s msgs=%d", st, n)
	}
	testFailUID = nil
	if err := imp.StartJob(ctx, jobOf(ctx, pool, acctID)); err != nil {
		t.Fatal(err)
	}
	runAll(t, imp, ctx, acctID)
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_import_failures WHERE account_id=$1`, acctID).Scan(&nRows)
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM mail_messages WHERE owner_user_id=$1 AND import_key IS NOT NULL`, target).Scan(&n)
	_ = pool.QueryRow(ctx, `SELECT msgs_failed FROM mail_import_accounts WHERE id=$1`, acctID).Scan(&failed)
	if nRows != 0 || failed != 0 || n != 2 {
		t.Fatalf("after retry: rows=%d failed=%d msgs=%d", nRows, failed, n)
	}
}

// TestLoginErrClass: login NO → permanent 专用密码 text; BAD → config text;
// a network error → not permanent (retry).
func TestLoginErrClass(t *testing.T) {
	noErr := &imap.Error{Type: imap.StatusResponseTypeNo, Text: "auth failed"}
	if m := loginErr(noErr); !strings.Contains(m, "专用密码") {
		t.Fatalf("NO: %q", m)
	}
	badErr := &imap.Error{Type: imap.StatusResponseTypeBad, Text: "bad cmd"}
	if m := loginErr(badErr); !strings.Contains(m, "拒绝登录命令") {
		t.Fatalf("BAD: %q", m)
	}
	if m := loginErr(errors.New("i/o timeout")); m != "" {
		t.Fatalf("io err should retry: %q", m)
	}
	// closed port → dial error → returns error (retryable), not permanent
	imp, ctx, pool, mk := testEnv(t)
	jobID, err := imp.CreateJob(ctx, JobParams{
		Name: "x", Host: "127.0.0.1", Port: 1, TLSMode: "plain", CreatedBy: mk(),
	})
	if err != nil {
		t.Fatal(err)
	}
	target := mk()
	_, _, errs := imp.AddAccountsCSV(ctx, jobID, "a@x,pw,"+emailOf(ctx, pool, target))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	var acctID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM mail_import_accounts WHERE job_id=$1`, jobID).Scan(&acctID)
	if err := imp.StartJob(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := imp.RunAccount(ctx, acctID, 5*time.Second); err == nil {
		t.Fatal("closed port should return an error (retryable)")
	}
}

// TestDeliverParsedSanitize: NUL + invalid UTF-8 in subject/body must not
// break the insert — pg text columns reject 0x00.
func TestDeliverParsedSanitize(t *testing.T) {
	_, ctx, pool, mk := testEnv(t)
	ms := mail.NewStore(pool, "wittyloong.com")
	target := mk()
	p := &mail.Parsed{
		From: "x@y", To: []string{emailOf(ctx, pool, target)},
		Subject: "bad\x00sub\xffject", Text: "ok\x00\xfftext",
		MessageID: "m\x00id",
	}
	id, err := ms.DeliverParsed(ctx, target, "inbox", nil, p, mail.DeliverOpts{})
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	var subj, body, mid string
	if err := pool.QueryRow(ctx,
		`SELECT subject,text_body,COALESCE(rfc_msgid,'') FROM mail_messages WHERE id=$1`, id).
		Scan(&subj, &body, &mid); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(subj, 0) || strings.ContainsRune(body, 0) || strings.ContainsRune(mid, 0) {
		t.Fatalf("NUL survived: %q %q %q", subj, body, mid)
	}
	if !strings.Contains(body, "�") {
		t.Fatalf("invalid utf8 not replaced: %q", body)
	}
}

// TestClaimKindsLanes: the exclusive claim (everything-but-import) returns
// the thumb task; the inclusive claim (import-only) returns the import.
func TestClaimKindsLanes(t *testing.T) {
	_, ctx, pool, _ := testEnv(t)
	obj, _ := objects.NewStore(pool, t.TempDir())
	// clean queue so no other test's rows interfere / 清空队列防止干扰
	_, _ = pool.Exec(ctx, `DELETE FROM bg_tasks WHERE status='queued'`)
	if err := obj.Enqueue(ctx, "mail_import", map[string]any{"account_id": uuid.Must(uuid.NewV7()).String()}); err != nil {
		t.Fatal(err)
	}
	if err := obj.Enqueue(ctx, "thumb", map[string]any{"object_id": uuid.Must(uuid.NewV7()).String()}); err != nil {
		t.Fatal(err)
	}
	// exclusive lane: skips mail_import → gets thumb
	got, err := obj.ClaimKinds(ctx, "w-main", []string{"mail_import"}, false)
	if err != nil || got == nil {
		t.Fatalf("exclusive claim: t=%v err=%v", got, err)
	}
	if got.Kind != "thumb" {
		t.Fatalf("exclusive claimed %s want thumb", got.Kind)
	}
	_ = obj.Done(ctx, got.ID)
	// inclusive lane: only mail_import
	got2, err := obj.ClaimKinds(ctx, "w-imp", []string{"mail_import"}, true)
	if err != nil || got2 == nil {
		t.Fatalf("inclusive claim: t=%v err=%v", got2, err)
	}
	if got2.Kind != "mail_import" {
		t.Fatalf("inclusive claimed %s want mail_import", got2.Kind)
	}
	_ = obj.Done(ctx, got2.ID)
	// Unfiltered Claim (no $2 bound) must still work — it once passed an
	// unreferenced arg and every claim failed. / 无过滤 Claim 曾多传参数致全失败。
	if err := obj.Enqueue(ctx, "gc", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	got3, err := obj.Claim(ctx, "w-any")
	if err != nil || got3 == nil || got3.Kind != "gc" {
		t.Fatalf("unfiltered claim: t=%v err=%v", got3, err)
	}
}
