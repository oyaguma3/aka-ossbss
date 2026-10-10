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
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

//go:embed templates static
var assets embed.FS

// DB は画面が使う DB の操作。*db.DB が満たす。
type DB interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
	AccountByEmail(ctx context.Context, email string) (db.Account, error)
}

// Provisioner は画面が使う provisioner の操作。*provisioner.Client が満たす。
type Provisioner interface {
	Status(ctx context.Context) (provisioner.Status, error)
}

// Handler は画面のハンドラー。
type Handler struct {
	log     *slog.Logger
	version string
	db      DB
	ident   auth.Identifier
	pv      Provisioner
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
	// Provisioner は eapaka-node-provisioner のクライアント。
	Provisioner Provisioner
}

// New は Handler を作る。テンプレートと静的ファイルはここで読み込む。
func New(opts Options) (*Handler, error) {
	if opts.Identifier == nil || opts.Provisioner == nil {
		return nil, fmt.Errorf("web: identifier and provisioner are required")
	}
	h := &Handler{log: opts.Log, version: opts.Version, db: opts.DB, ident: opts.Identifier, pv: opts.Provisioner}
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

// homeData はトップの画面（利用者と、その人ができることの要約、DB と provisioner の状態）に出す値。
type homeData struct {
	DBReachable   bool
	SchemaVersion int
	Permissions   []permission
	// Provisioner は provisioner の状態（運用担当とシステム管理者だけ。設計概要 §6.2 の「provisioner の状態の閲覧」）。
	Provisioner *provisionerData
}

// provisionerData は provisioner の状態のカードに出す値。
type provisionerData struct {
	Status provisioner.Status
	// Error は provisioner に接続できない・エラーを返したときの文（Hint は原因の見当）。
	Error, Hint string
}

// provisionerStatus は provisioner の状態を取得する。下流が止まっていると時間がかかるので、上限を短くする。
func (h *Handler) provisionerStatus(r *http.Request) *provisionerData {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := h.pv.Status(ctx)
	switch {
	case err != nil && provisioner.IsUnavailable(err):
		h.log.Warn("provisioner is not available", "error", err)
		return &provisionerData{Error: "provisioner に接続できません。", Hint: provisioner.Diagnose(err)}
	case err != nil:
		h.log.Warn("provisioner status", "error", err)
		return &provisionerData{Error: "provisioner がエラーを返しました。"}
	case !st.IsProvisioner():
		return &provisionerData{Error: "接続先は provisioner ではないようです（本PoCの provisioning-api など）。OSSBSS_PROVISIONER_URL を確かめてください。"}
	}
	return &provisionerData{Status: st}
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
	u := userFrom(r.Context())
	d := homeData{Permissions: permissions(u.Actor())}
	// 上限時間は呼び出しごとに取る（provisioner の応答を待つ間に、DB の確認の時間を使い切らないように）。
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	v, err := h.db.SchemaVersion(ctx)
	cancel()
	if err != nil {
		h.log.Warn("home: db is not reachable", "error", err)
	} else {
		d.DBReachable, d.SchemaVersion = true, v
	}
	if authz.Allowed(u.Actor(), authz.ProvisionerView, authz.NoTenant) {
		d.Provisioner = h.provisionerStatus(r)
	}
	h.render(w, r, http.StatusOK, "home", "ダッシュボード", d)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "ページが見つかりません。")
}
