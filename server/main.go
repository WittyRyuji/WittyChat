// WittyChat server entrypoint.
// Commands: serve (default) | bootstrap | migrate
//
// WittyChat 服务端入口。
// 子命令：serve（默认）| bootstrap（首个管理员）| migrate（应用数据库迁移）。
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"wittychat/core/agents"
	"wittychat/core/audit"
	"wittychat/core/auth"
	"wittychat/core/db"
	"wittychat/core/docs"
	"wittychat/core/hr"
	"wittychat/core/im"
	"wittychat/core/mail"
	"wittychat/core/migrations"
	"wittychat/core/objects"
	"wittychat/core/settings"
	"wittychat/server/httpapi"
	"wittychat/server/realtime"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 && os.Args[1][0] != '-' {
		cmd = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}

	switch cmd {
	case "bootstrap":
		cmdBootstrap()
	case "migrate":
		cmdMigrate()
	case "gencert":
		cmdGenCert()
	case "dkim":
		cmdDKIM()
	case "serve":
		cmdServe()
	default:
		fmt.Fprintln(os.Stderr, "usage: server [serve|bootstrap|migrate]")
		os.Exit(2)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func dbURL() string {
	return envOr("WITTYCHAT_DB_URL", "postgres://wittychat:wittychat@localhost:5432/wittychat?sslmode=disable")
}

func publicURL() string {
	return envOr("WITTYCHAT_PUBLIC_URL", "http://localhost:8080")
}

// officeConfig wires the OnlyOffice integration from env. Disabled unless
// WITTYCHAT_ONLYOFFICE_URL is set — the docs container is a separate
// docker service (deploy/docker-compose*.yml). When unset, we probe the
// standard local dev address (localhost:8180 healthcheck) so `docker
// Office is OFF unless WITTYCHAT_ONLYOFFICE_URL is set. Dev convenience:
// WITTYCHAT_ONLYOFFICE_AUTODETECT=1 explicitly opts into probing
// localhost:8180 — an opt-in env var, not silent magic. The JWT secret is
// ALWAYS required (no public default baked into the binary): the save
// callback overwrites object bytes and a public default secret would let
// anyone forge it.
// 未配置 URL 即关闭。开发便捷项：WITTYCHAT_ONLYOFFICE_AUTODETECT=1 显式
// 探测 localhost:8180——显式开关而非静默魔法。JWT 密钥始终必填（二进制
// 内无公开默认值）：保存回调覆盖对象字节，公开默认密钥等于放伪造。
//
//	WITTYCHAT_ONLYOFFICE_URL            browser-facing docs base (api.js)
//	WITTYCHAT_ONLYOFFICE_JWT_SECRET     shared HS256 secret (required)
//	WITTYCHAT_ONLYOFFICE_AUTODETECT     =1 → probe localhost:8180 when URL unset
//	WITTYCHAT_ONLYOFFICE_CALLBACK_BASE  how the container reaches US
//	                                    (Docker Desktop: host.docker.internal)
//	WITTYCHAT_ONLYOFFICE_INTERNAL_URL   where WE fetch saved output
//	                                    (default = DOCS_URL; compose: http://onlyoffice)
func officeConfig() *httpapi.OfficeConfig {
	docs := envOr("WITTYCHAT_ONLYOFFICE_URL", "")
	secret := os.Getenv("WITTYCHAT_ONLYOFFICE_JWT_SECRET")
	if docs == "" {
		if os.Getenv("WITTYCHAT_ONLYOFFICE_AUTODETECT") != "1" ||
			!probeDocsServer("http://localhost:8180") {
			return nil
		}
		docs = "http://localhost:8180"
		log.Printf("onlyoffice: auto-detected docs server at %s (AUTODETECT=1)", docs)
	}
	if secret == "" {
		// The save callback overwrites object bytes — without the shared
		// JWT secret anyone could forge it. Fail closed.
		// 保存回调会覆盖对象字节——无共享 JWT 密钥即可被伪造，直接禁用。
		log.Printf("onlyoffice: WITTYCHAT_ONLYOFFICE_JWT_SECRET unset — office integration DISABLED")
		return nil
	}
	fetch := envOr("WITTYCHAT_ONLYOFFICE_INTERNAL_URL", docs)
	return &httpapi.OfficeConfig{
		DocsURL:      docs,
		JWTSecret:    []byte(secret),
		CallbackBase: envOr("WITTYCHAT_ONLYOFFICE_CALLBACK_BASE", "http://host.docker.internal:8080"),
		FetchBase:    fetch,
	}
}

// probeDocsServer hits /healthcheck — the docs server answers "true".
// 探活：OnlyOffice /healthcheck 返回 "true"，其它服务不会误配。
func probeDocsServer(base string) bool {
	cli := &http.Client{Timeout: 2 * time.Second}
	res, err := cli.Get(base + "/healthcheck")
	if err != nil {
		return false
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64))
	return res.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "true"
}

func openDB() *pgxpool.Pool {
	pool, err := db.Open(context.Background(), dbURL())
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	return pool
}

// ---------- serve ----------

func cmdServe() {
	pool := openDB()
	defer pool.Close()

	dataDir := envOr("WITTYCHAT_DATA_DIR", "./data")
	key, keySrc, err := auth.LoadOrCreateSecretKey(dataDir)
	if err != nil {
		log.Fatalf("secret key: %v", err)
	}
	switch keySrc {
	case "created":
		log.Printf("generated %s/secret.key (0600) — back it up; it seals TOTP seeds etc.", dataDir)
	case "file":
		log.Printf("secret key loaded from %s/secret.key", dataDir)
	}

	imStore := im.NewStore(pool)
	objStore, err := objects.NewStore(pool, envOr("WITTYCHAT_DATA_DIR", "./data"))
	if err != nil {
		log.Fatalf("objects: %v", err)
	}
	objStore.EnableS3(objects.NewS3(objects.S3ConfigFromEnv(),
		envOr("WITTYCHAT_DATA_DIR", "./data")))
	// Background dims sweep: re-sniffs every unchecked image row (legacy
	// uploads, multipart completes, and rows sized by the retired
	// orientation-blind request-path sniff). Cancelled on shutdown.
	// 后台尺寸清扫：重嗅所有未检查图片行（旧上传、分段上传、以及被
	// 已下线的请求路径嗅探填错方向的行）。随关停取消。
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go objStore.RunDimsBackfill(bgCtx, 10*time.Minute)
	settingsStore := settings.New(pool)
	imStore.RecallWindow = time.Duration(
		settingsStore.Int(context.Background(), settings.KeyIMRecallTimeout, settings.DefRecallTimeout)) * time.Second
	d := &httpapi.Deps{
		Pool:        pool,
		Store:       auth.NewStore(pool),
		Audit:       audit.New(pool),
		Limiter:     auth.NewLimiter(int(settingsStore.Int(context.Background(), settings.KeySecurityLockN, settings.DefLockThreshold))),
		AcctLimiter: auth.NewLimiter(int(settingsStore.Int(context.Background(), settings.KeySecurityLockN, settings.DefLockThreshold)) * 2),
		SecretKey:   key,
		Issuer:      envOr("WITTYCHAT_ISSUER", "WittyChat"),
		PublicURL:   publicURL(),
		// Behind our nginx the client's X-Forwarded-For last hop is real.
		// 生产走 nginx：可信代理追加的 XFF 最右一跳才是真实来源。
		TrustProxy: envOr("WITTYCHAT_TRUST_PROXY", "") == "1" || envOr("WITTYCHAT_TRUST_PROXY", "") == "true",
		IM:         imStore,
		Hub:        realtime.NewHub(imStore.IsMember),
		Objects:    objStore,
		Settings:   settingsStore,
		Signer:     objects.NewSigner(key),
		Mail:       mail.NewStore(pool, envOr("WITTYCHAT_MAIL_DOMAIN", "wittyloong.com")),
		Agents:     agents.NewStore(pool),
		HR:         hr.New(pool),
		Docs:       docs.New(pool, imStore.IsMember, imStore.MemberRole),
		Office:     officeConfig(),
	}

	addr := envOr("WITTYCHAT_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(d),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		cert, kfile := os.Getenv("WITTYCHAT_TLS_CERT"), os.Getenv("WITTYCHAT_TLS_KEY")
		if cert != "" && kfile != "" {
			log.Printf("server listening on %s (TLS)", addr)
			if err := srv.ListenAndServeTLS(cert, kfile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("listen tls: %v", err)
			}
			return
		}
		log.Printf("server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// ---------- bootstrap ----------

func cmdBootstrap() {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	email := fs.String("email", "", "admin email (required) / 管理员邮箱（必填）")
	name := fs.String("name", "Admin", "display name / 显示名")
	pw := fs.String("password", "", "dev shortcut: set password directly, skip link / 开发捷径：直接设密码跳过激活链接")
	_ = fs.Parse(os.Args[1:])
	if *email == "" {
		fs.Usage()
		os.Exit(2)
	}

	pool := openDB()
	defer pool.Close()
	store := auth.NewStore(pool)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, *email, *name, true)
	if err != nil {
		log.Fatalf("create admin: %v", err)
	}

	if *pw != "" {
		phc, err := auth.HashPassword(*pw)
		if err != nil {
			log.Fatalf("hash: %v", err)
		}
		if err := store.SetPassword(ctx, u.ID, phc); err != nil {
			log.Fatalf("set password: %v", err)
		}
		if err := store.ActivateUser(ctx, u.ID); err != nil {
			log.Fatalf("activate: %v", err)
		}
		fmt.Printf("admin %s created and activated with the given password (dev only!)\n", u.Email)
		return
	}

	token, expiresAt, err := store.CreateActivationLink(ctx, u.ID, auth.LinkKindActivation, u.ID)
	if err != nil {
		log.Fatalf("activation link: %v", err)
	}
	fmt.Printf("admin %s created. Activation link (expires %s):\n\n  %s/activate?token=%s\n\n",
		u.Email, expiresAt.Format(time.RFC3339), publicURL(), token)
}

// ---------- migrate ----------

func cmdMigrate() {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dir := fs.String("dir", "up", "up|down N|goto N|version")
	steps := fs.Int("steps", 1, "steps for down")
	_ = fs.Parse(os.Args[1:])

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		log.Fatalf("migration source: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dbURL())
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	switch *dir {
	case "up":
		err = m.Up()
	case "down":
		err = m.Steps(-*steps)
	case "version":
		var v uint
		var dirty bool
		v, dirty, err = m.Version()
		if err == nil {
			fmt.Printf("version %d (dirty=%v)\n", v, dirty)
		}
	default:
		log.Fatalf("unknown -dir %q", *dir)
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		log.Fatalf("migrate %s: %v", *dir, err)
	}
}

// ---------- gencert ----------

// cmdGenCert writes a self-signed ECDSA cert for dev/LAN TLS into
// WITTYCHAT_DATA_DIR/tls/{cert,key}.pem (localhost + hostname SANs).
// Production TLS terminates at nginx (see deploy/nginx.conf).
// 生成开发/内网自签证书；生产由 nginx 终结 TLS。
func cmdGenCert() {
	fs := flag.NewFlagSet("gencert", flag.ExitOnError)
	out := fs.String("out", "", "tls dir (default DATA_DIR/tls)")
	host := fs.String("host", "", "extra SAN hostnames, comma-separated")
	days := fs.Int("days", 825, "validity days")
	_ = fs.Parse(os.Args[1:])

	dir := *out
	if dir == "" {
		dir = filepath.Join(envOr("WITTYCHAT_DATA_DIR", "./data"), "tls")
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	hosts := []string{"localhost"}
	if h, err := os.Hostname(); err == nil {
		hosts = append(hosts, h)
	}
	if *host != "" {
		hosts = append(hosts, strings.Split(*host, ",")...)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "WittyChat Dev", Organization: []string{"WittyChat"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(0, 0, *days),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              hosts,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	if err != nil {
		log.Fatalf("cert: %v", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cp, certPEM, 0o600); err != nil {
		log.Fatalf("cert write: %v", err)
	}
	if err := os.WriteFile(kp, keyPEM, 0o600); err != nil {
		log.Fatalf("key write: %v", err)
	}
	fmt.Printf("self-signed cert written:\n  %s\n  %s\n\nserve with:\n  WITTYCHAT_TLS_CERT=%s WITTYCHAT_TLS_KEY=%s server.exe serve\n",
		cp, kp, cp, kp)
}

// cmdDKIM generates an RSA signing key, stores it in workspace_settings, and
// prints the DNS TXT record to publish. Private key never leaves the DB/host.
// 生成 DKIM RSA 密钥存入 workspace_settings 并打印待发布的 DNS TXT 记录；
// 私钥不出库不出机。
func cmdDKIM() {
	fs := flag.NewFlagSet("dkim", flag.ExitOnError)
	selector := fs.String("selector", "s1", "DKIM selector")
	domain := fs.String("domain", envOr("WITTYCHAT_MAIL_DOMAIN", ""), "mail domain (default: WITTYCHAT_MAIL_DOMAIN)")
	bits := fs.Int("bits", 2048, "RSA key bits")
	_ = fs.Parse(os.Args[1:])

	pool := openDB()
	defer pool.Close()
	ctx := context.Background()
	if *domain == "" {
		log.Fatal("mail domain empty — pass -domain or set WITTYCHAT_MAIL_DOMAIN")
	}

	key, err := rsa.GenerateKey(rand.Reader, *bits)
	if err != nil {
		log.Fatalf("keygen: %v", err)
	}
	privPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	// updated_by is FK to users; CLI runs before/alongside any admin user, so
	// insert with NULL via direct SQL.
	for _, kv := range [][2]string{
		{settings.KeyMailDKIMKey, privPEM},
		{settings.KeyMailDKIMSelector, *selector},
	} {
		_, err = pool.Exec(ctx,
			`INSERT INTO workspace_settings (key, value, updated_at)
			 VALUES ($1, to_jsonb($2::text), now())
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
			kv[0], kv[1])
		if err != nil {
			log.Fatalf("store %s: %v", kv[0], err)
		}
	}

	pubDER := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	p := base64.StdEncoding.EncodeToString(pubDER)
	fmt.Printf("DKIM key stored (selector %s, domain %s).\n\nPublish this DNS TXT record:\n\n  %s._domainkey.%s  TXT  \"v=DKIM1; k=rsa; p=%s\"\n\nAlso ensure SPF includes your outbound IP, e.g.:\n  %s  TXT  \"v=spf1 mx ~all\"\n",
		*selector, *domain, *selector, *domain, p, *domain)
}
