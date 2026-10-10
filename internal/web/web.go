// Package web は本BFF の画面（Go + htmx + daisyUI）。
//
// 同梱しているサードパーティのファイル（static/）:
//   - htmx.min.js: htmx 2.0.11（https://htmx.org、0BSD）
//   - app.css: Tailwind CSS（MIT）と daisyUI（MIT）で生成する。リポジトリには入れず、scripts/build-css.sh で作る
//     （Docker のビルドの段でも同じスクリプトを使う。設計概要 §15 の 9）。
package web

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/auth"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/db"
)

//go:embed templates static
var assets embed.FS

// DB は画面が使う DB の操作。*db.DB が満たす。
type DB interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
	AccountByEmail(ctx context.Context, email string) (db.Account, error)
}

// Handler は画面のハンドラー。
type Handler struct {
	log     *slog.Logger
	version string
	db      DB
	ident   auth.Identifier
	pages   pages
	static  *staticFiles
}

// Options は Handler の設定。
type Options struct {
	Log *slog.Logger
	// Version は画面のフッターに出すバージョン。
	Version string
	DB      DB
	// Identifier は要求から利用者を決める（Cloudflare Access の JWT、または開発用の設定）。
	Identifier auth.Identifier
}

// New は Handler を作る。テンプレートと静的ファイルはここで読み込む。
func New(opts Options) (*Handler, error) {
	if opts.Identifier == nil {
		return nil, fmt.Errorf("web: identifier is required")
	}
	h := &Handler{log: opts.Log, version: opts.Version, db: opts.DB, ident: opts.Identifier}
	var err error
	if h.pages, err = parsePages(); err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	if h.static, err = newStaticFiles(http.HandlerFunc(h.notFound)); err != nil {
		return nil, fmt.Errorf("load static files: %w", err)
	}
	return h, nil
}

// Routes はルーティングとミドルウェアを組み立てた http.Handler を返す。
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", h.static)
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /{$}", h.home)
	mux.HandleFunc("/", h.notFound)

	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.log.Warn("cross-origin request rejected", "method", r.Method, "path", r.URL.Path, "origin", r.Header.Get("Origin"))
		h.renderError(w, r, http.StatusForbidden, "この操作は受け付けられません。画面を開き直してから操作してください。")
	}))
	return h.accessLog(securityHeaders(cop.Handler(h.authenticate(mux))))
}

// healthz は DB に接続できれば 200 を返す（compose の healthcheck などに使う。認証の対象外）。
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := h.db.Ping(ctx); err != nil {
		h.log.Warn("healthz: db is not reachable", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "db is not reachable")
		return
	}
	fmt.Fprintln(w, "ok")
}

// homeData はトップの画面（ステップ 2 では、利用者と、その人ができることの要約、DB の状態）に出す値。
type homeData struct {
	DBReachable   bool
	SchemaVersion int
	Permissions   []permission
}

// permission は、利用者ができる操作の 1 行。
type permission struct {
	Label string
	// Scope は範囲（できなければ空）。
	Scope string
}

// permissions は利用者ができること・できないことの一覧を作る（設計概要 §6.2）。
func permissions(a authz.Actor) []permission {
	// 範囲を確かめる対象のテナント: A・B は自分のテナント、C・D はテナントなし（C・D はどのテナントでも同じ）。
	target := authz.NoTenant
	if a.Role.TenantScoped() {
		target = a.TenantID
	}
	var out []permission
	for _, op := range authz.Ops() {
		p := permission{Label: op.Label()}
		switch {
		case !authz.Allowed(a, op, target):
		case op == authz.AccountManage && a.Role == authz.TenantAdmin:
			p.Scope = "自分のテナントのテナントユーザー"
		case a.Role.TenantScoped() && op.Scoped():
			p.Scope = "自分のテナントのもの"
		default:
			p.Scope = "すべて"
		}
		out = append(out, p)
	}
	return out
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	u := userFrom(r.Context())
	d := homeData{Permissions: permissions(u.Actor())}
	v, err := h.db.SchemaVersion(ctx)
	if err != nil {
		h.log.Warn("home: db is not reachable", "error", err)
	} else {
		d.DBReachable, d.SchemaVersion = true, v
	}
	h.render(w, r, http.StatusOK, "home", "ダッシュボード", d)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "ページが見つかりません。")
}
