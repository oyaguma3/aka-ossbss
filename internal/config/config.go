// Package config は環境変数（接頭辞 OSSBSS_）から設定を読む。
package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
)

// Config は aka-ossbss の設定。
type Config struct {
	// Addr は HTTP の待ち受けアドレス（cloudflared からだけ受ける）。
	Addr     string
	LogLevel slog.Level

	// DB は PostgreSQL への接続。
	DB DB
	// Provisioner は aka-provisioner への接続。
	Provisioner Provisioner

	// AccessTeamDomain は Cloudflare Access のチームのドメイン（例 example.cloudflareaccess.com）。
	// AccessAUDs は Access のアプリの AUD タグ（カンマ区切りで複数）。両方を与えると Access の JWT で利用者を識別する。
	AccessTeamDomain string
	AccessAUDs       []string
	// InitialAdminEmail は最初のシステム管理者のメールアドレス（正規化したもの）。
	InitialAdminEmail string
	// AuditDir は監査ログの日ごとのファイル（JSONL）の書き出し先（設計概要 §14・§18）。
	AuditDir string
	// KEKFile は一括投入の待ちのファイルを暗号化する鍵（KEK）のファイル（設計概要 §10.1・§18）。
	KEKFile string
	// BulkConcurrency は一括投入の同時の呼び出しの数（設計概要 §10.3）。
	BulkConcurrency int
	// DevUserEmail は開発用の設定（もとの設計概要 §15 の 10）。JWT の検証の代わりに、このメールアドレスの利用者として動かす。
	// Addr がループバックのときだけ使え、Access の設定とは同時に使えない。
	DevUserEmail string
}

// DevMode は開発用の設定で動くか。
func (c Config) DevMode() bool { return c.DevUserEmail != "" }

// Provisioner は provisioner への接続の設定。
type Provisioner struct {
	// URL は provisioner のベース URL。既定は同一ホストの共有ネットワーク（aka-provisioner）の名前。
	URL string
	// ClientCertFile は aka-ossbss のクライアント証明書（gen-client-cert の出力。秘密鍵も含めてよい）。
	// ClientKeyFile は秘密鍵（空なら ClientCertFile から読む）。
	ClientCertFile string
	ClientKeyFile  string
	// ServerCertFile は provisioner のサーバー証明書（provisioner の server-cert の出力）。
	ServerCertFile string
	// Timeout は 1 回の呼び出しの上限時間。
	Timeout time.Duration
}

// DB は PostgreSQL への接続の設定。
type DB struct {
	// Addr は「ホスト:ポート」。
	Addr     string
	Name     string
	User     string
	Password string
	// MaxConns は接続プールの上限。
	MaxConns int32
}

// maxBulkConcurrency は OSSBSS_BULK_CONCURRENCY の上限（DB の接続を使い切らないように）。
const maxBulkConcurrency = 8

// Load は環境変数から設定を読む。誤りはまとめて返す。
func Load() (Config, error) {
	c := Config{
		Addr: cmp.Or(os.Getenv("OSSBSS_ADDR"), ":8446"),
		Provisioner: Provisioner{
			URL:            cmp.Or(os.Getenv("OSSBSS_PROVISIONER_URL"), "https://aka-provisioner:9446/admin/v2"),
			ClientCertFile: cmp.Or(os.Getenv("OSSBSS_PROVISIONER_CLIENT_CERT"), "/certs/provisioner-client.pem"),
			ClientKeyFile:  os.Getenv("OSSBSS_PROVISIONER_CLIENT_KEY"),
			ServerCertFile: cmp.Or(os.Getenv("OSSBSS_PROVISIONER_SERVER_CERT"), "/certs/provisioner-server.pem"),
			Timeout:        15 * time.Second,
		},
		AuditDir:        cmp.Or(os.Getenv("OSSBSS_AUDIT_DIR"), "/data/audit"),
		KEKFile:         cmp.Or(os.Getenv("OSSBSS_KEK_FILE"), "/run/secrets/kek"),
		BulkConcurrency: 5,
		DB: DB{
			Addr:     cmp.Or(os.Getenv("OSSBSS_DB_ADDR"), "postgres:5432"),
			Name:     cmp.Or(os.Getenv("OSSBSS_DB_NAME"), "ossbss"),
			User:     cmp.Or(os.Getenv("OSSBSS_DB_USER"), "ossbss"),
			Password: os.Getenv("OSSBSS_DB_PASSWORD"),
		},
	}
	var errs []error
	if v := os.Getenv("OSSBSS_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("OSSBSS_LOG_LEVEL: %w", err))
		}
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		errs = append(errs, fmt.Errorf("OSSBSS_ADDR: %w", err))
	}
	if _, _, err := net.SplitHostPort(c.DB.Addr); err != nil {
		errs = append(errs, fmt.Errorf("OSSBSS_DB_ADDR: %w", err))
	}
	// 画面に加えて、一括投入の 2 本の待ち行列（対象ごとに同時 5 件。行の処理の間 1 つ使う。ロックに 1 つ）が使う。
	c.DB.MaxConns = 20
	if v := os.Getenv("OSSBSS_DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > 100 {
			errs = append(errs, errors.New("OSSBSS_DB_MAX_CONNS: must be an integer between 1 and 100"))
		}
		c.DB.MaxConns = int32(n)
	}
	if v := os.Getenv("OSSBSS_BULK_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxBulkConcurrency {
			errs = append(errs, fmt.Errorf("OSSBSS_BULK_CONCURRENCY: must be an integer between 1 and %d", maxBulkConcurrency))
		}
		c.BulkConcurrency = n
	}
	if u, err := url.Parse(c.Provisioner.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, errors.New("OSSBSS_PROVISIONER_URL: must be an https URL such as https://aka-provisioner:9446/admin/v2"))
	}
	if v := os.Getenv("OSSBSS_PROVISIONER_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > 5*time.Minute {
			errs = append(errs, errors.New("OSSBSS_PROVISIONER_TIMEOUT: must be a duration between 1s and 5m (e.g. 15s)"))
		}
		c.Provisioner.Timeout = d
	}
	if c.DB.Password == "" {
		errs = append(errs, errors.New("OSSBSS_DB_PASSWORD is not set"))
	}
	errs = append(errs, c.loadAuth()...)
	return c, errors.Join(errs...)
}

// hostnamePattern はチームのドメインの形式。
var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// loadAuth は利用者の識別の設定を読む。Access と開発用の設定のどちらか一方だけを受け付ける。
func (c *Config) loadAuth() []error {
	var errs []error
	c.InitialAdminEmail = authz.NormalizeEmail(os.Getenv("OSSBSS_INITIAL_ADMIN_EMAIL"))
	switch {
	case c.InitialAdminEmail == "":
		errs = append(errs, errors.New("OSSBSS_INITIAL_ADMIN_EMAIL is not set"))
	case !authz.ValidEmail(c.InitialAdminEmail):
		errs = append(errs, errors.New("OSSBSS_INITIAL_ADMIN_EMAIL: must be an email address of up to 64 characters "+
			"using only letters, digits and ._- (it is passed to the provisioner as X-Operator-Id)"))
	}

	c.AccessTeamDomain = strings.ToLower(strings.TrimSpace(os.Getenv("OSSBSS_ACCESS_TEAM_DOMAIN")))
	for v := range strings.SplitSeq(os.Getenv("OSSBSS_ACCESS_AUD"), ",") {
		if v = strings.TrimSpace(v); v != "" {
			c.AccessAUDs = append(c.AccessAUDs, v)
		}
	}
	c.DevUserEmail = authz.NormalizeEmail(os.Getenv("OSSBSS_DEV_USER_EMAIL"))
	access := c.AccessTeamDomain != "" || len(c.AccessAUDs) > 0

	switch {
	case access && c.DevUserEmail != "":
		errs = append(errs, errors.New("OSSBSS_DEV_USER_EMAIL cannot be used with the Cloudflare Access settings (OSSBSS_ACCESS_*)"))
	case access:
		if c.AccessTeamDomain == "" || len(c.AccessAUDs) == 0 {
			errs = append(errs, errors.New("both OSSBSS_ACCESS_TEAM_DOMAIN and OSSBSS_ACCESS_AUD are required"))
		} else if !hostnamePattern.MatchString(c.AccessTeamDomain) {
			errs = append(errs, errors.New("OSSBSS_ACCESS_TEAM_DOMAIN: must be a host name such as example.cloudflareaccess.com"))
		}
	case c.DevUserEmail != "":
		if !authz.ValidEmail(c.DevUserEmail) {
			errs = append(errs, errors.New("OSSBSS_DEV_USER_EMAIL: must be an email address of up to 64 characters using only letters, digits and ._-"))
		}
		// 開発用の設定は、他のホストから届かない待ち受けでだけ使う（もとの設計概要 §15 の 10、§17.2）。
		if host, _, err := net.SplitHostPort(c.Addr); err == nil && !isLoopback(host) {
			errs = append(errs, fmt.Errorf("OSSBSS_DEV_USER_EMAIL can be used only when OSSBSS_ADDR is a loopback address (got %q)", c.Addr))
		}
	default:
		errs = append(errs, errors.New("set OSSBSS_ACCESS_TEAM_DOMAIN and OSSBSS_ACCESS_AUD (Cloudflare Access), "+
			"or OSSBSS_DEV_USER_EMAIL for development"))
	}
	return errs
}

// isLoopback は、待ち受けのホストがループバックか（空や 0.0.0.0 はすべてのアドレスなので違う）。
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
