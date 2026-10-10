package web

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/db"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/trace"
)

// テナントの範囲の判定、所属の表示、二重送信の防止、監査ログ（画面仕様 §1）。

// dbTimeout は画面の DB の操作 1 回の上限時間。
const dbTimeout = 5 * time.Second

// dbContext は DB の操作 1 回の上限時間つきのコンテキストを返す
// （provisioner の応答を待つ間に使い切らないよう、呼び出しごとに取る）。
func dbContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), dbTimeout)
}

// dbUnavailable は DB を使えないときの 503 を返す。
func (h *Handler) dbUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("db", "path", r.URL.Path, "error", err)
	h.renderError(w, r, http.StatusServiceUnavailable, "データベースに接続できません。しばらくしてから開き直してください。")
}

// ---- テナントの範囲 ----

// subscriberOutOfScope は、A・B が範囲の外の加入者を開いたときの文（存在しないときと同じ。画面仕様 §1.2）。
func subscriberOutOfScope(imsi string) string {
	return "加入者 " + imsi + " は見つかりません（このテナントに属していないか、登録されていません）。"
}

// clientOutOfScope は、A・B が範囲の外の RADIUSクライアントを開いたときの文。
func clientOutOfScope(id int64) string {
	return "RADIUSクライアント #" + strconv.FormatInt(id, 10) + " は見つかりません（このテナントに属していないか、登録されていません）。"
}

// subscriberScope は IMSI の所属を引き、利用者がその加入者に op をしてよいかを確かめる（要求ごとに必ず通す）。
// 範囲の外なら 404、範囲の中でも役割にない操作なら 403 を返して false。
func (h *Handler) subscriberScope(w http.ResponseWriter, r *http.Request, imsi string, op authz.Op) (int64, bool) {
	ctx, cancel := dbContext(r)
	tenant, err := h.db.SubscriberTenant(ctx, imsi)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return 0, false
	}
	a := userFrom(r.Context()).Actor()
	if !authz.Allowed(a, authz.SubscriberView, tenant) {
		h.renderErrorLink(w, r, http.StatusNotFound, subscriberOutOfScope(imsi), "/subscribers", "加入者の一覧へ")
		return 0, false
	}
	if !authz.Allowed(a, op, tenant) {
		h.forbidden(w, r)
		return 0, false
	}
	return tenant, true
}

// clientScope は RADIUSクライアントの所属を引き、利用者が op をしてよいかを確かめる（subscriberScope と同じ）。
func (h *Handler) clientScope(w http.ResponseWriter, r *http.Request, id int64, op authz.Op) (int64, bool) {
	ctx, cancel := dbContext(r)
	tenant, err := h.db.ClientTenant(ctx, id)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return 0, false
	}
	a := userFrom(r.Context()).Actor()
	if !authz.Allowed(a, authz.ClientView, tenant) {
		h.renderErrorLink(w, r, http.StatusNotFound, clientOutOfScope(id), "/clients", "RADIUSクライアントの一覧へ")
		return 0, false
	}
	if !authz.Allowed(a, op, tenant) {
		h.forbidden(w, r)
		return 0, false
	}
	return tenant, true
}

// pathIMSI は URL の IMSI を返す。形式が違えば 404 を返して false。
func (h *Handler) pathIMSI(w http.ResponseWriter, r *http.Request) (string, bool) {
	imsi := r.PathValue("imsi")
	if !imsiPattern.MatchString(imsi) {
		h.notFound(w, r)
		return "", false
	}
	return imsi, true
}

// pathClientID は URL の RADIUSクライアントの ID を返す。形式が違えば 404 を返して false。
func (h *Handler) pathClientID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return 0, false
	}
	return id, true
}

// ---- テナントの名前と絞り込み ----

// tenantInfo はテナントの一覧と、ID から名前を引く表。
type tenantInfo struct {
	List  []db.Tenant
	names map[int64]string
}

// Name はテナントの名前を返す（0 は「テナントなし」）。
func (t tenantInfo) Name(id int64) string {
	if id == authz.NoTenant {
		return "テナントなし"
	}
	if n, ok := t.names[id]; ok {
		return n
	}
	return "#" + strconv.FormatInt(id, 10)
}

// Has は、そのテナントがあるか。
func (t tenantInfo) Has(id int64) bool {
	_, ok := t.names[id]
	return ok
}

// tenants はテナントの一覧を読む。
func (h *Handler) tenants(r *http.Request) (tenantInfo, error) {
	ctx, cancel := dbContext(r)
	defer cancel()
	list, err := h.db.Tenants(ctx)
	if err != nil {
		return tenantInfo{}, err
	}
	t := tenantInfo{List: list, names: make(map[int64]string, len(list))}
	for _, x := range list {
		t.names[x.ID] = x.Name
	}
	return t, nil
}

// tenantFilter は一覧の絞り込み（C・D のテナントの選択）。A・B は自分のテナントに固定する。
type tenantFilter struct {
	// Value は画面の選択の値（"" はすべて、"none" はテナントなし、それ以外はテナントの ID）。
	Value string
	// All はすべて、None はテナントなし。どちらでもなければ Tenant のものに絞る。
	All, None bool
	Tenant    int64
	// Selectable は、利用者がテナントを選べるか（C・D）。
	Selectable bool
}

// filterNone は「テナントなし」の絞り込みの値。
const filterNone = "none"

// readTenantFilter は ?tenant= を読む。A・B は自分のテナントに固定する。値が正しくなければ false。
func readTenantFilter(r *http.Request, t tenantInfo) (tenantFilter, bool) {
	a := userFrom(r.Context()).Actor()
	if a.Role.TenantScoped() {
		return tenantFilter{Tenant: a.TenantID}, true
	}
	v := r.URL.Query().Get("tenant")
	f := tenantFilter{Value: v, Selectable: true}
	switch v {
	case "":
		f.All = true
	case filterNone:
		f.None = true
	default:
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || !t.Has(id) {
			f.Value, f.All = "", true
			return f, false
		}
		f.Tenant = id
	}
	return f, true
}

// readTenantChoice は登録のフォームの所属のテナント（tenant）を読む。"none" はテナントなし。
// 選んでいない・形式が違えば ok が false、選んだテナントがなければ exists が false。
func readTenantChoice(v string, t tenantInfo) (id int64, ok, exists bool) {
	if v == filterNone {
		return authz.NoTenant, true, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 1 {
		return 0, false, false
	}
	return id, true, t.Has(id)
}

// ---- 二重送信の防止 ----

// idemKeyPattern は、フォームに持たせる Idempotency-Key の形式（crypto/rand.Text の出力）。
var idemKeyPattern = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// newIdemKey は、フォームを描くたびに作る Idempotency-Key を返す（画面仕様 §1.5）。
// 同じフォームを送り直しても（応答を待たずに押し直した、タイムアウトの後に送り直した）、provisioner は最初の結果を返す。
func newIdemKey() string { return rand.Text() }

// formIdemKey は、送られてきたフォームの Idempotency-Key（項目 name）を返す（形式が違えば空。空なら付けずに送る）。
func formIdemKey(r *http.Request, name string) string {
	if k := r.PostForm.Get(name); idemKeyPattern.MatchString(k) {
		return k
	}
	return ""
}

// keepIdemKey は、失敗の後にフォームを描き直すときの Idempotency-Key を返す。
// provisioner に届かなかった（接続できない・タイムアウト）場合は、同じキーで送り直せるよう残す
// （provisioner は 5xx を覚えないので、届いて失敗していても同じキーでやり直せる）。
// provisioner が 4xx を返した場合は、その応答を覚えているので、新しいキーにする（入力を直して送ると内容が変わるため）。
func keepIdemKey(err error, key string) string {
	if key != "" && provisioner.IsUnavailable(err) {
		return key
	}
	return newIdemKey()
}

// ---- 監査ログ ----

// 本BFF の監査ログの操作の名前（画面仕様 §1.7）。
const (
	auditSubscriberCreate   = "subscriber.create"
	auditSubscriberUpdate   = "subscriber.update"
	auditSubscriberDelete   = "subscriber.delete"
	auditSubscriberKeysRead = "subscriber.keys.read"
	auditPolicyPut          = "policy.put"
	auditPolicySuspend      = "policy.suspend"
	auditPolicyResume       = "policy.resume"
	auditClientCreate       = "client.create"
	auditClientUpdate       = "client.update"
	auditClientDelete       = "client.delete"
	auditClientSecretRead   = "client.secret.read"
	auditAssignmentUpdate   = "assignment.update"
	auditAssignmentDelete   = "assignment.delete"
	auditTenantCreate       = "tenant.create"
	auditTenantUpdate       = "tenant.update"
	auditTenantDelete       = "tenant.delete"
	auditAccountCreate      = "account.create"
	auditAccountUpdate      = "account.update"
	auditAccountDisable     = "account.disable"
	auditAccountEnable      = "account.enable"
	auditAccountDelete      = "account.delete"
	auditOperationRetry     = "operation.retry"
	auditOperationDismiss   = "operation.dismiss"
)

// record は、成功した操作を本BFF の監査ログに残す。tenant は対象のテナント（テナントなしは 0）。
// detail に秘密の値（Ki / OPc、共有シークレット）を入れてはならない。書き込みに失敗しても操作は済んでいるので、
// ERROR のログを出して続ける（画面仕様 §1.7）。
func (h *Handler) record(r *http.Request, action, target string, tenant int64, detail map[string]any) {
	u := userFrom(r.Context())
	e := db.AuditEntry{ActorEmail: u.Email, ActorRole: u.Role, TenantID: tenant, Action: action, Target: target,
		Detail: maps.Clone(detail), TraceID: trace.From(r.Context()), Remote: h.ident.ClientIP(r)}
	// 要求が取り消されても（利用者が画面を閉じても）、済んだ操作の記録は残す。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), dbTimeout)
	defer cancel()
	if err := h.db.InsertAudit(ctx, e); err != nil {
		h.log.Error("audit log write failed", "action", action, "target", target, "user", u.Email,
			"trace_id", e.TraceID, "error", err)
	}
}

// recordMove は、テナントが from から to に変わる操作（所属の付け替え、アカウントのテナントの変更）を監査ログに残す。
// 移動元と移動先が別のテナントなら、それぞれのテナントに 1 行ずつ書く（行は対象のテナントを 1 つしか持たないので、
// 移動元のテナント管理者にも見えるように。テナントなしの側には書かない。§17.5 の 3）。両方テナントなしなら 1 行。
func (h *Handler) recordMove(r *http.Request, action, target string, from, to int64, detail map[string]any) {
	switch {
	case from == to || from == authz.NoTenant:
		h.record(r, action, target, to, detail)
	case to == authz.NoTenant:
		h.record(r, action, target, from, detail)
	default:
		h.record(r, action, target, from, detail)
		h.record(r, action, target, to, detail)
	}
}

// clientTarget は監査ログの対象に書く RADIUSクライアントの表記。
func clientTarget(id int64) string { return "#" + strconv.FormatInt(id, 10) }

// ---- 並行の取得 ----

// fetchConcurrency は、テナントの範囲の一覧で provisioner から同時に取得する数（設計概要 §15 の 7）。
const fetchConcurrency = 5

// forEachConcurrently は f(0)〜f(n-1) を、同時に limit 件までで呼ぶ。
func forEachConcurrently(n, limit int, f func(i int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			f(i)
		})
	}
	wg.Wait()
}

// isNotFound は、provisioner が対象（加入者・RADIUSクライアント・認可ポリシー）がないと返したか
// （aka-only-server を扱わない設定の 404 は含めない）。
func isNotFound(err error) bool {
	e, ok := errors.AsType[*provisioner.Error](err)
	return ok && e.Status == http.StatusNotFound && e.Problem.Cause != provisioner.CauseDownstreamNotConfigured
}
