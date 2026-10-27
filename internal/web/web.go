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
	SubscriberTenant(ctx context.Context, imsi string) (int64, error)
	SubscriberTenants(ctx context.Context, imsis []string) (map[string]int64, error)
	TenantSubscribers(ctx context.Context, tenantID int64, prefix, after string, limit int) ([]string, error)
	CountTenantSubscribers(ctx context.Context, tenantID int64, prefix string) (int64, error)
	AssignSubscriber(ctx context.Context, imsi string, tenantID int64, by string) error
	ClientTenant(ctx context.Context, clientID int64) (int64, error)
	ClientTenants(ctx context.Context) (map[int64]int64, error)
	AssignClient(ctx context.Context, clientID, tenantID int64, by string) error
	InsertAudit(ctx context.Context, e db.AuditEntry) error
	ListAudit(ctx context.Context, q db.AuditQuery) ([]db.AuditRecord, error)
	SubscriberAssignments(ctx context.Context, after string, limit int) ([]db.Assignment, error)
	ClientAssignments(ctx context.Context) ([]db.Assignment, error)
	TenantSummaries(ctx context.Context) ([]db.TenantSummary, error)
	TenantSummary(ctx context.Context, id int64) (db.TenantSummary, error)
	CreateTenant(ctx context.Context, name string) (db.Tenant, error)
	RenameTenant(ctx context.Context, id int64, name string) error
	DeleteTenant(ctx context.Context, id int64) error
	Accounts(ctx context.Context, f db.AccountFilter) ([]db.Account, error)
	AccountByID(ctx context.Context, id int64) (db.Account, error)
	CreateAccount(ctx context.Context, email string, role authz.Role, tenantID int64, by string) (int64, error)
	UpdateAccount(ctx context.Context, id int64, from, to db.AccountState) error
	SetAccountDisabled(ctx context.Context, id int64, from db.AccountState, disabled bool) error
	DeleteAccount(ctx context.Context, id int64, from db.AccountState) error
}

// Provisioner は画面が使う provisioner の操作。*provisioner.Client が満たす。
type Provisioner interface {
	Status(ctx context.Context) (provisioner.Status, error)
	ListSubscribers(ctx context.Context, p provisioner.ListParams) (provisioner.SubscriberList, error)
	GetSubscriber(ctx context.Context, imsi string) (provisioner.Subscriber, error)
	CreateSubscriber(ctx context.Context, s provisioner.SubscriberCreate, idempotencyKey string) (provisioner.Subscriber, error)
	UpdateSubscriber(ctx context.Context, imsi string, u provisioner.SubscriberUpdate, idempotencyKey string) (provisioner.Subscriber, error)
	DeleteSubscriber(ctx context.Context, imsi string, idempotencyKey string) error
	GetSubscriberKeys(ctx context.Context, imsi string) (provisioner.SubscriberKeys, error)
	PutPolicy(ctx context.Context, imsi string, p provisioner.PolicyPut, idempotencyKey string) (provisioner.Policy, bool, error)
	SetPolicyStatus(ctx context.Context, imsi, status, idempotencyKey string) (provisioner.Policy, error)
	ListRADIUSClients(ctx context.Context) ([]provisioner.RADIUSClient, error)
	CreateRADIUSClient(ctx context.Context, rc provisioner.RADIUSClientCreate, idempotencyKey string) (provisioner.RADIUSClient, error)
	GetRADIUSClient(ctx context.Context, id int64) (provisioner.RADIUSClient, error)
	UpdateRADIUSClient(ctx context.Context, id int64, u provisioner.RADIUSClientUpdate, idempotencyKey string) (provisioner.RADIUSClient, error)
	DeleteRADIUSClient(ctx context.Context, id int64, idempotencyKey string) error
	GetRADIUSClientSecret(ctx context.Context, id int64) (string, error)
	ListSessions(ctx context.Context, p provisioner.SessionParams) (provisioner.SessionList, error)
	ListPolicies(ctx context.Context, p provisioner.ListParams) (provisioner.PolicyList, error)
	ListOperations(ctx context.Context, status string, limit int) (provisioner.OperationList, error)
	GetOperation(ctx context.Context, id string) (provisioner.Operation, error)
	RetryOperation(ctx context.Context, id, idempotencyKey string) (provisioner.Operation, error)
	DismissOperation(ctx context.Context, id, idempotencyKey string) (provisioner.Operation, error)
	ListAuditLogs(ctx context.Context, p provisioner.AuditLogParams) (provisioner.AuditLogList, error)
	ListProvAuditLogs(ctx context.Context, p provisioner.AuditLogParams) (provisioner.ProvAuditLogList, error)
	ListAkaAuditLogs(ctx context.Context, p provisioner.AuditLogParams) (provisioner.AkaAuditLogList, error)
}

// Handler は画面のハンドラー。
type Handler struct {
	log     *slog.Logger
	version string
	db      DB
	ident   auth.Identifier
	pv      Provisioner
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
	// InitialAdminEmail は最初のシステム管理者のメールアドレス（OSSBSS_INITIAL_ADMIN_EMAIL）。
	InitialAdminEmail string
}

// New は Handler を作る。テンプレートと静的ファイルはここで読み込む。
func New(opts Options) (*Handler, error) {
	if opts.Identifier == nil || opts.Provisioner == nil {
		return nil, fmt.Errorf("web: identifier and provisioner are required")
	}
	h := &Handler{log: opts.Log, version: opts.Version, db: opts.DB, ident: opts.Identifier, pv: opts.Provisioner,
		initialAdmin: authz.NormalizeEmail(opts.InitialAdminEmail)}
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

	// 加入者・停止と再開・認可ポリシー（画面仕様 §2、§3）。
	mux.HandleFunc("GET /subscribers", h.subscribers)
	mux.HandleFunc("GET /subscribers/new", h.subscriberNew)
	mux.HandleFunc("POST /subscribers/new/form", h.subscriberNewForm)
	mux.HandleFunc("POST /subscribers", h.subscriberCreate)
	mux.HandleFunc("GET /subscribers/{imsi}", h.subscriber)
	mux.HandleFunc("POST /subscribers/{imsi}/keys", h.subscriberKeys)
	mux.HandleFunc("POST /subscribers/{imsi}/auth", h.subscriberAuth)
	mux.HandleFunc("POST /subscribers/{imsi}/delete", h.subscriberDelete)
	mux.HandleFunc("POST /subscribers/{imsi}/status", h.subscriberStatus)
	mux.HandleFunc("GET /subscribers/{imsi}/policy", h.policy)
	mux.HandleFunc("POST /subscribers/{imsi}/policy/edit", h.policyEdit)
	mux.HandleFunc("POST /subscribers/{imsi}/policy", h.policySave)
	mux.HandleFunc("POST /subscribers/{imsi}/policy/status", h.policyStatus)

	// RADIUSクライアント（画面仕様 §4）。
	mux.HandleFunc("GET /clients", h.clients)
	mux.HandleFunc("POST /clients", h.clientCreate)
	mux.HandleFunc("GET /clients/{id}", h.client)
	mux.HandleFunc("POST /clients/{id}/update", h.clientUpdate)
	mux.HandleFunc("POST /clients/{id}/secret", h.clientSecret)
	mux.HandleFunc("POST /clients/{id}/delete", h.clientDelete)

	// セッション（画面仕様 §5）。
	mux.HandleFunc("GET /sessions", h.sessions)

	// ダッシュボードの停止中の数（画面仕様 §6）。
	mux.HandleFunc("GET /dashboard/suspended", h.dashboardSuspended)

	// 所属の付け替えと点検（画面仕様 §7）。
	mux.HandleFunc("POST /subscribers/{imsi}/tenant", h.subscriberTenantChange)
	mux.HandleFunc("POST /subscribers/assign", h.subscribersAssign)
	mux.HandleFunc("POST /clients/{id}/tenant", h.clientTenantChange)
	mux.HandleFunc("POST /clients/assign", h.clientsAssign)
	mux.HandleFunc("GET /assignments/orphans", h.orphans)
	mux.HandleFunc("POST /assignments/orphans/delete", h.orphansDelete)

	// テナント（画面仕様 §8）。
	mux.HandleFunc("GET /tenants", h.tenantList)
	mux.HandleFunc("POST /tenants", h.tenantCreate)
	mux.HandleFunc("GET /tenants/{id}", h.tenant)
	mux.HandleFunc("POST /tenants/{id}/rename", h.tenantRename)
	mux.HandleFunc("POST /tenants/{id}/delete", h.tenantDelete)

	// アカウント（画面仕様 §9）。
	mux.HandleFunc("GET /accounts", h.accounts)
	mux.HandleFunc("POST /accounts", h.accountCreate)
	mux.HandleFunc("GET /accounts/{id}", h.account)
	mux.HandleFunc("POST /accounts/{id}/update", h.accountUpdate)
	mux.HandleFunc("POST /accounts/{id}/disable", h.accountDisable)
	mux.HandleFunc("POST /accounts/{id}/enable", h.accountEnable)
	mux.HandleFunc("POST /accounts/{id}/delete", h.accountDelete)

	// 操作の記録（画面仕様 §10）。
	mux.HandleFunc("GET /operations", h.operations)
	mux.HandleFunc("GET /operations/{id}", h.operation)
	mux.HandleFunc("POST /operations/{id}/retry", h.operationRetry)
	mux.HandleFunc("POST /operations/{id}/dismiss", h.operationDismiss)

	// 監査ログ（画面仕様 §11）。
	mux.HandleFunc("GET /audit", h.audit)
	mux.HandleFunc("GET /audit/provisioner", h.pvAudit)
	mux.HandleFunc("GET /audit/prov", h.provAudit)
	mux.HandleFunc("GET /audit/aka", h.akaAudit)
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

// homeData はトップの画面（ダッシュボード。画面仕様 §6）に出す値。
type homeData struct {
	DBReachable   bool
	SchemaVersion int
	Permissions   []permission
	// Counts は利用者の範囲の件数（取得できなかったものは負の値）。
	Counts dashboardCounts
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
	d.Counts = h.dashboardCounts(r)
	if authz.Allowed(u.Actor(), authz.ProvisionerView, authz.NoTenant) {
		d.Provisioner = h.provisionerStatus(r)
	}
	h.render(w, r, http.StatusOK, "home", "ダッシュボード", d)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "ページが見つかりません。")
}
