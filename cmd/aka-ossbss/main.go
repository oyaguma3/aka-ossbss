// aka-ossbss は MVP セットの、運用者とテナントの利用者が使う画面と管理面の業務の処理（台帳など）を受け持つノード
// （aka-provisioner の管理クライアント）。
package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// distroless のイメージにはタイムゾーンのデータがないので、埋め込んで環境変数 TZ を効かせる。
	_ "time/tzdata"

	"github.com/oyaguma3/aka-ossbss/internal/access"
	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/certs"
	"github.com/oyaguma3/aka-ossbss/internal/config"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/server"
	"github.com/oyaguma3/aka-ossbss/internal/web"
)

// version はビルド時に -ldflags "-X main.version=..." で埋め込む。
var version = "dev"

const usage = `usage: aka-ossbss <command>

commands:
  serve              サーバーを起動する（コマンド省略時の既定）。起動時に DB のスキーマを移行する
  migrate            DB のスキーマを移行して終わる
  healthcheck        動いている aka-ossbss の /healthz を確かめる（compose の healthcheck 用）
  check-provisioner  aka-provisioner に接続できるか確かめる
  gen-client-cert    aka-provisioner に提示するクライアント証明書と秘密鍵を作る
                     （aka-ossbss gen-client-cert -h で使い方を表示）
  gen-kek [-out ファイル]  一括投入の待ちのファイルを暗号化する KEK を作る（-out を省くと標準出力）

設定は環境変数（接頭辞 OSSBSS_）で与える。
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = migrate(ctx)
	case "healthcheck":
		err = healthcheck(ctx)
	case "check-provisioner":
		err = checkProvisioner(ctx)
	case "gen-client-cert":
		err = genClientCert(args[1:], os.Stdout, os.Stderr)
	case "gen-kek":
		err = genKEK(args[1:], os.Stdout, os.Stderr)
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openDB は設定を読み、DB に接続してスキーマを移行する。
func openDB(ctx context.Context) (config.Config, *slog.Logger, *db.DB, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, nil, nil, err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("starting", "version", version)

	// DB のコンテナの起動を待つため、接続は少しの間やり直す。
	var d *db.DB
	deadline := time.Now().Add(30 * time.Second)
	for {
		openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		d, err = db.Open(openCtx, db.Options{Addr: cfg.DB.Addr, Name: cfg.DB.Name, User: cfg.DB.User,
			Password: cfg.DB.Password, MaxConns: cfg.DB.MaxConns})
		cancel()
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		log.Warn("db is not reachable; retrying", "addr", cfg.DB.Addr, "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return cfg, log, nil, err
	}
	migCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	applied, err := d.Migrate(migCtx)
	if err != nil {
		d.Close()
		return cfg, log, nil, err
	}
	v, err := d.SchemaVersion(migCtx)
	if err != nil {
		d.Close()
		return cfg, log, nil, err
	}
	log.Info("database", "addr", cfg.DB.Addr, "name", cfg.DB.Name, "schema_version", v, "applied", applied)
	return cfg, log, d, nil
}

func serve(ctx context.Context) error {
	cfg, log, d, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer d.Close()

	// 最初のシステム管理者（もとの設計概要 §15 の 12）。役割を変えられていたり無効にされていたりしたら戻す。
	res, err := d.EnsureInitialAdmin(ctx, cfg.InitialAdminEmail)
	if err != nil {
		return err
	}
	if res == db.InitialAdminRestored {
		log.Warn("restored the initial system admin to an enabled system admin", "email", cfg.InitialAdminEmail)
	} else {
		log.Info("initial system admin", "email", cfg.InitialAdminEmail, "result", string(res))
	}

	ident, err := newIdentifier(ctx, cfg, log)
	if err != nil {
		return err
	}

	pv, err := newProvisioner(cfg, log)
	if err != nil {
		return err
	}
	// 監査ログの日ごとのファイル（設計概要 §14）。書けないまま動き出さないよう、起動のときに確かめる。
	if err := audit.CheckWritableDir(cfg.AuditDir); err != nil {
		log.Error("audit directory is not writable", "event_id", "AUDIT_DIR_ERR", "error", err)
		return err
	}
	auditFiles := audit.NewFiles(cfg.AuditDir, auditRetentionDays)
	bgCtx, stopBG := context.WithCancel(ctx)
	defer stopBG()
	go maintainAudit(bgCtx, log, auditFiles)
	log.Info("provisioner client", "url", pv.BaseURL(), "client_cert_fingerprint", certs.Fingerprint(pv.ClientCertificate()),
		"client_cert_not_after", pv.ClientCertificate().NotAfter, "timeout", cfg.Provisioner.Timeout.String())
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	checkAtStartup(checkCtx, log, pv)
	cancel()

	h, err := web.New(web.Options{Log: log, Version: version, DB: d, Identifier: ident, Provisioner: pv,
		AuditFiles: auditFiles, InitialAdminEmail: cfg.InitialAdminEmail})
	if err != nil {
		return err
	}
	return server.Run(ctx, server.Options{Addr: cfg.Addr, Handler: h.Routes(), Log: log,
		// 画面の 1 回の要求で aka-provisioner を何度か呼ぶ（両側の操作、結果の読み直し）ので、呼び出しの上限より十分に長くする。
		WriteTimeout: max(90*time.Second, 4*cfg.Provisioner.Timeout+30*time.Second)})
}

const (
	// auditRetentionDays は監査ログの日ごとのファイルを持つ日数（ほかのノードと同じ。PostgreSQL の監査ログは 13 か月）。
	auditRetentionDays = 14
	// auditMaintainInterval は監査ログのファイルを圧縮・削除する間隔。
	auditMaintainInterval = time.Hour
)

// maintainAudit は、起動のときと 1 時間ごとに、監査ログの前の日までのファイルを圧縮し、古いものを消す。
func maintainAudit(ctx context.Context, log *slog.Logger, files *audit.Files) {
	t := time.NewTicker(auditMaintainInterval)
	defer t.Stop()
	for {
		if err := files.Maintain(time.Now()); err != nil {
			log.Warn("audit file maintenance failed", "event_id", "AUDIT_FILE_ERR", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// newIdentifier は、要求から利用者を決める方法を作る（Cloudflare Access の JWT、または開発用の設定）。
func newIdentifier(ctx context.Context, cfg config.Config, log *slog.Logger) (auth.Identifier, error) {
	if cfg.DevMode() {
		log.Warn("development mode: requests are not authenticated; every request acts as the development user "+
			"(OSSBSS_DEV_USER_EMAIL). Do not use this setting in production", "email", cfg.DevUserEmail, "addr", cfg.Addr)
		return auth.Dev(cfg.DevUserEmail), nil
	}
	v, err := access.New(access.Options{TeamDomain: cfg.AccessTeamDomain, AUDs: cfg.AccessAUDs,
		HTTPClient: &http.Client{Timeout: 10 * time.Second}})
	if err != nil {
		return nil, err
	}
	// 公開鍵を先に取得しておく。取得できなくても起動は続ける（最初の要求のときにもう一度試みる）。
	if n, err := v.Refresh(ctx); err != nil {
		log.Warn("cannot fetch Cloudflare Access public keys; will retry on the first request", "url", v.CertsURL(), "error", err)
	} else {
		log.Info("cloudflare access", "team_domain", cfg.AccessTeamDomain, "auds", len(cfg.AccessAUDs), "keys", n)
	}
	return auth.Access(v), nil
}

func migrate(ctx context.Context) error {
	_, _, d, err := openDB(ctx)
	if err != nil {
		return err
	}
	d.Close()
	return nil
}

// healthcheck は、同じコンテナで動いている aka-ossbss の /healthz（DB に接続できるか）を確かめる。
// distroless のイメージには curl がないので、compose の healthcheck はこれを使う。
func healthcheck(ctx context.Context) error {
	_, port, err := net.SplitHostPort(cmp.Or(os.Getenv("OSSBSS_ADDR"), ":8446"))
	if err != nil {
		return fmt.Errorf("OSSBSS_ADDR: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
