// Package web は aka-ossbss の画面（Go + htmx + daisyUI）。
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

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

//go:embed templates static
var assets embed.FS

// DB は画面が使う DB の操作。*db.DB が満たす。
type DB interface {
	Ping(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
	AccountByEmail(ctx context.Context, email string) (db.Account, error)
	Tenants(ctx context.Context) ([]db.Tenant, error)
	Subscriber(ctx context.Context, imsi string) (db.Entry, error)
	ListSubscribers(ctx context.Context, f db.SubscriberFilter, after string, limit int) ([]db.Entry, error)
	CountSubscribers(ctx context.Context, f db.SubscriberFilter) (int64, error)
	SubscriberSources(ctx context.Context, tenantID int64) ([]string, error)
	CountLedger(ctx context.Context, tenantID int64) (db.LedgerCounts, error)
	WithSubscriber(ctx context.Context, imsi string, meta db.Meta, fn func(db.Ledger) error) error
	InsertAudit(ctx context.Context, e db.AuditEntry) (int64, time.Time, error)
	ListAudit(ctx context.Context, q db.AuditQuery) ([]db.AuditRecord, error)
	TenantSummaries(ctx context.Context) ([]db.TenantSummary, error)
	TenantSummary(ctx context.Context, id int64) (db.TenantSummary, error)
	CreateTenant(ctx context.Context, name string, meta db.Meta) (db.Tenant, error)
	RenameTenant(ctx context.Context, id int64, name string, meta db.Meta) (string, error)
	DeleteTenant(ctx context.Context, id int64, meta db.Meta) error
	Accounts(ctx context.Context, f db.AccountFilter) ([]db.Account, error)
	AccountByID(ctx context.Context, id int64) (db.Account, error)
	CreateAccount(ctx context.Context, email string, role authz.Role, tenantID int64, by string) (int64, error)
	UpdateAccount(ctx context.Context, id int64, from, to db.AccountState) error
	SetAccountDisabled(ctx context.Context, id int64, from db.AccountState, disabled bool) error
	DeleteAccount(ctx context.Context, id int64, from db.AccountState) error
}

// Provisioner は画面が使う aka-provisioner の口。*provisioner.Client が満たす。
type Provisioner interface {
	Status(ctx context.Context) (provisioner.Status, error)
	VecgenSubscriber(ctx context.Context, imsi string) (provisioner.VecgenSubscriber, error)
	VecgenBatchGet(ctx context.Context, imsis []string) (provisioner.VecgenBatch, error)
	CreateVecgenSubscriber(ctx context.Context, s provisioner.VecgenSubscriberCreate, idempotencyKey string) (provisioner.VecgenSubscriber, error)
	UpdateVecgenSubscriber(ctx context.Context, imsi string, u provisioner.VecgenSubscriberUpdate, idempotencyKey string) (provisioner.VecgenSubscriber, error)
	VecgenClients(ctx context.Context) ([]provisioner.VecgenClient, error)
	RadiusSubscriber(ctx context.Context, imsi string) (provisioner.RadiusSubscriber, error)
	RadiusBatchGet(ctx context.Context, imsis []string) (provisioner.RadiusBatch, error)
	CreateRadiusSubscriber(ctx context.Context, s provisioner.RadiusSubscriberCreate, idempotencyKey string) (provisioner.RadiusSubscriber, error)
	UpdateRadiusSubscriber(ctx context.Context, imsi string, u provisioner.RadiusSubscriberUpdate, idempotencyKey string) (provisioner.RadiusSubscriber, error)
	RadiusSources(ctx context.Context) ([]provisioner.Source, error)
	Sessions(ctx context.Context, p provisioner.SessionParams) (provisioner.SessionList, error)
	SetState(ctx context.Context, d provisioner.Downstream, imsi string, ch provisioner.StateChange, idempotencyKey string) (string, error)
	Reinstate(ctx context.Context, d provisioner.Downstream, imsi, reason, idempotencyKey string) (string, error)
	DeleteSubscriber(ctx context.Context, d provisioner.Downstream, imsi, idempotencyKey string) error
	AuditLogs(ctx context.Context, src provisioner.AuditSource, p provisioner.AuditLogParams) (provisioner.AuditLogList, error)
}

// Handler は画面のハンドラー。
type Handler struct {
	log     *slog.Logger
	version string
	db      DB
	ident   auth.Identifier
	pv      Provisioner
	// audit は監査ログの日ごとのファイル（nil なら書かない）。
	audit *audit.Files
	// initialAdmin は最初のシステム管理者のメールアドレス（画面からの削除・降格・無効化を断る）。
	initialAdmin string
	pages        pages
	static       *staticFiles
}

// Options は Handler の設定。
type Options struct {
	Log *slog.Logger
	// Version は画面のフッターに出すバージョン。
	Version string
	DB      DB
	// Identifier は要求から利用者を決める（Cloudflare Access の JWT、または開発用の設定）。
	Identifier auth.Identifier
	// Provisioner は aka-provisioner のクライアント。
	Provisioner Provisioner
	// AuditFiles は監査ログの日ごとのファイル（OSSBSS_AUDIT_DIR。nil なら書かない）。
	AuditFiles *audit.Files
	// InitialAdminEmail は最初のシステム管理者のメールアドレス（OSSBSS_INITIAL_ADMIN_EMAIL）。
	InitialAdminEmail string
}

// New は Handler を作る。テンプレートと静的ファイルはここで読み込む。
func New(opts Options) (*Handler, error) {
	if opts.Identifier == nil || opts.Provisioner == nil {
		return nil, fmt.Errorf("web: identifier and provisioner are required")
	}
	h := &Handler{log: opts.Log, version: opts.Version, db: opts.DB, ident: opts.Identifier, pv: opts.Provisioner,
		audit: opts.AuditFiles, initialAdmin: authz.NormalizeEmail(opts.InitialAdminEmail)}
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

	// 加入者（設計概要 §7・§8。段階 3-1 の W4〜W8）。
	mux.HandleFunc("GET /subscribers", h.subscribers)
	mux.HandleFunc("GET /subscribers/new", h.subscriberNew)
	mux.HandleFunc("POST /subscribers/new/vecgen", h.vecgenCreate)
	mux.HandleFunc("POST /subscribers/new/radius", h.radiusCreate)
	mux.HandleFunc("GET /subscribers/{imsi}", h.subscriber)
	mux.HandleFunc("POST /subscribers/{imsi}/state", h.subscriberState)
	mux.HandleFunc("POST /subscribers/{imsi}/reinstate", h.subscriberReinstate)
	mux.HandleFunc("POST /subscribers/{imsi}/delete", h.subscriberDelete)
	mux.HandleFunc("POST /subscribers/{imsi}/vecgen", h.vecgenUpdate)
	mux.HandleFunc("POST /subscribers/{imsi}/source", h.sourceUpdate)
	mux.HandleFunc("POST /subscribers/{imsi}/tenant", h.tenantMove)
	mux.HandleFunc("GET /subscribers/{imsi}/policy", h.policy)
	mux.HandleFunc("POST /subscribers/{imsi}/policy/edit", h.policyEdit)
	mux.HandleFunc("POST /subscribers/{imsi}/policy", h.policySave)

	// セッション（設計概要 §11。W9）。
	mux.HandleFunc("GET /sessions", h.sessions)

	// テナント。
	mux.HandleFunc("GET /tenants", h.tenantList)
	mux.HandleFunc("POST /tenants", h.tenantCreate)
	mux.HandleFunc("GET /tenants/{id}", h.tenant)
	mux.HandleFunc("POST /tenants/{id}/rename", h.tenantRename)
	mux.HandleFunc("POST /tenants/{id}/delete", h.tenantDelete)

	// アカウント。
	mux.HandleFunc("GET /accounts", h.accounts)
	mux.HandleFunc("POST /accounts", h.accountCreate)
	mux.HandleFunc("GET /accounts/{id}", h.account)
	mux.HandleFunc("POST /accounts/{id}/update", h.accountUpdate)
	mux.HandleFunc("POST /accounts/{id}/disable", h.accountDisable)
	mux.HandleFunc("POST /accounts/{id}/enable", h.accountEnable)
	mux.HandleFunc("POST /accounts/{id}/delete", h.accountDelete)

	// 監査ログ（設計概要 §14。W10・W11）。
	mux.HandleFunc("GET /audit", h.auditList)
	mux.HandleFunc("GET /audit/{source}", h.downstreamAudit)
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

// homeData はトップの画面（ダッシュボード。設計概要 §15）に出す値。
type homeData struct {
	DBReachable   bool
	SchemaVersion int
	Permissions   []permission
	// Staff は C・D（全テナントの件数と、aka-provisioner と下流の状態を出す）。
	Staff bool
	// Tenants はテナントの数（C・D。数えられなければ -1）。
	Tenants int
	// Counts は台帳の件数（A・B は自分のテナント）。数えられなければ nil。
	Counts *db.LedgerCounts
	// Provisioner は aka-provisioner と下流の状態（C・D）。
	Provisioner *provisionerData
}

// provisionerData は aka-provisioner の状態のカードに出す値。
type provisionerData struct {
	Status provisioner.Status
	// Error は aka-provisioner に接続できない・誤りを返したときの文（Hint は原因の見当）。
	Error, Hint string
}

// provisionerStatus は aka-provisioner の状態を取得する（下流の状態は aka-provisioner が 30 秒ごとに集めたもの）。
func (h *Handler) provisionerStatus(r *http.Request) *provisionerData {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st, err := h.pv.Status(ctx)
	switch {
	case err != nil && provisioner.IsUnavailable(err):
		h.log.Warn("provisioner is not available", "error", err)
		return &provisionerData{Error: "aka-provisioner に接続できません。", Hint: provisioner.Diagnose(err)}
	case err != nil:
		h.log.Warn("provisioner status", "error", err)
		return &provisionerData{Error: "aka-provisioner が誤りを返しました（OSSBSS_PROVISIONER_URL が /admin/v2 を指しているか確かめてください）。"}
	case !st.IsProvisioner():
		return &provisionerData{Error: "接続先は aka-provisioner 2.0.0 ではないようです。OSSBSS_PROVISIONER_URL を確かめてください。"}
	}
	return &provisionerData{Status: st}
}

// permission は、利用者ができる操作の 1 行。
type permission struct {
	Label string
	// Scope は範囲（できなければ空）。
	Scope string
}

// permissions は利用者ができること・できないことの一覧を作る（設計概要 §5）。
func permissions(a authz.Actor) []permission {
	// 範囲を確かめる対象のテナント: A・B は自分のテナント、C・D はテナントに属さないものとして判定する（C・D はどれでも同じ）。
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
	a := u.Actor()
	d := homeData{Permissions: permissions(a), Staff: !a.Role.TenantScoped(), Tenants: -1}
	// 上限時間は呼び出しごとに取る（aka-provisioner の応答を待つ間に、DB の確認の時間を使い切らないように）。
	ctx, cancel := dbContext(r)
	if v, err := h.db.SchemaVersion(ctx); err != nil {
		h.log.Warn("home: db is not reachable", "error", err)
	} else {
		d.DBReachable, d.SchemaVersion = true, v
		if c, err := h.db.CountLedger(ctx, a.TenantID); err != nil {
			h.log.Warn("home: count ledger", "error", err)
		} else {
			d.Counts = &c
		}
		if d.Staff {
			if t, err := h.db.Tenants(ctx); err == nil {
				d.Tenants = len(t)
			}
		}
	}
	cancel()
	if authz.Allowed(a, authz.ProvisionerView, authz.NoTenant) {
		d.Provisioner = h.provisionerStatus(r)
	}
	h.render(w, r, http.StatusOK, "home", "ダッシュボード", d)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "ページが見つかりません。")
}
