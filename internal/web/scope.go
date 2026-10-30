package web

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

// テナントの範囲の判定、テナントの名前と絞り込み、二重送信の防止、監査ログ（もとの画面仕様 §1）。

// dbTimeout は画面の DB の操作 1 回の上限時間。
const dbTimeout = 5 * time.Second

// dbContext は DB の操作 1 回の上限時間つきのコンテキストを返す
// （aka-provisioner の応答を待つ間に使い切らないよう、呼び出しごとに取る）。
func dbContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), dbTimeout)
}

// dbUnavailable は DB を使えないときの 503 を返す。
func (h *Handler) dbUnavailable(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("db", "path", r.URL.Path, "error", err)
	h.renderError(w, r, http.StatusServiceUnavailable, "データベースに接続できません。しばらくしてから開き直してください。")
}

// can は、利用者の役割に op があるか（A・B は自分のテナントのものとして判定する。画面に出すかなどに使う）。
func can(r *http.Request, op authz.Op) bool {
	a := userFrom(r.Context()).Actor()
	return authz.Allowed(a, op, a.TenantID)
}

// isStaff は、利用者が運用担当・システム管理者か（全テナントを扱い、AMF・SQN や aka-provisioner の状態を見る）。
func isStaff(r *http.Request) bool {
	return !userFrom(r.Context()).Role.TenantScoped()
}

// ---- テナントの範囲 ----

// subscriberOutOfScope は、範囲の外の加入者を開いたときの文（存在しないときと同じ。もとの画面仕様 §1.2）。
func subscriberOutOfScope(imsi string) string {
	return "加入者 " + imsi + " は見つかりません（このテナントに属していないか、登録されていません）。"
}

// subscriberScope は IMSI の台帳の行を引き、利用者がその加入者に op をしてよいかを確かめる（要求ごとに必ず通す）。
// 台帳にない・範囲の外なら 404、範囲の中でも役割にない操作なら 403 を返して false。
func (h *Handler) subscriberScope(w http.ResponseWriter, r *http.Request, imsi string, op authz.Op) (db.Entry, bool) {
	ctx, cancel := dbContext(r)
	e, err := h.db.Subscriber(ctx, imsi)
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, subscriberOutOfScope(imsi), "/subscribers", "加入者の一覧へ")
		return db.Entry{}, false
	case err != nil:
		h.dbUnavailable(w, r, err)
		return db.Entry{}, false
	}
	a := userFrom(r.Context()).Actor()
	if !authz.Allowed(a, authz.SubscriberView, e.TenantID) {
		h.renderErrorLink(w, r, http.StatusNotFound, subscriberOutOfScope(imsi), "/subscribers", "加入者の一覧へ")
		return db.Entry{}, false
	}
	if !authz.Allowed(a, op, e.TenantID) {
		h.forbidden(w, r)
		return db.Entry{}, false
	}
	return e, true
}

// requireOp は、範囲を絞らない操作（または A・B の自分のテナントの操作）の権限を確かめる。なければ 403 を返して false。
func (h *Handler) requireOp(w http.ResponseWriter, r *http.Request, op authz.Op) bool {
	if !can(r, op) {
		h.forbidden(w, r)
		return false
	}
	return true
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

// ---- テナントの名前と絞り込み ----

// tenantInfo はテナント（削除していないもの）の一覧と、ID から名前を引く表。
type tenantInfo struct {
	List  []db.Tenant
	names map[int64]string
}

// Name はテナントの名前を返す（0 は「なし」、削除したテナントは番号）。
func (t tenantInfo) Name(id int64) string {
	if id == authz.NoTenant {
		return "なし"
	}
	if n, ok := t.names[id]; ok {
		return n
	}
	return "#" + strconv.FormatInt(id, 10)
}

// Has は、そのテナントがあるか（削除していないか）。
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
	// Value は画面の選択の値（"" はすべて、"none" はテナントに属さないもの、それ以外はテナントの ID）。
	Value string
	// All はすべて、None はテナントに属さないもの。どちらでもなければ Tenant のものに絞る。
	All, None bool
	Tenant    int64
	// Selectable は、利用者がテナントを選べるか（C・D）。AllowNone は「なし」を選べるか（アカウントと監査ログ）。
	Selectable, AllowNone bool
}

// filterNone は「テナントに属さないもの」の絞り込みの値。
const filterNone = "none"

// readTenantFilter は ?tenant= を読む。A・B は自分のテナントに固定する。allowNone なら「なし」も受け付ける。
// 値が正しくなければ false。
func readTenantFilter(r *http.Request, t tenantInfo, allowNone bool) (tenantFilter, bool) {
	a := userFrom(r.Context()).Actor()
	if a.Role.TenantScoped() {
		return tenantFilter{Tenant: a.TenantID}, true
	}
	v := r.URL.Query().Get("tenant")
	f := tenantFilter{Value: v, Selectable: true, AllowNone: allowNone}
	switch {
	case v == "":
		f.All = true
	case v == filterNone && allowNone:
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

// readTenantChoice はフォームのテナント（登録・付け替えの先）を読む。選んでいない・形式が違えば ok が false、
// 選んだテナントがなければ（削除済みを含む）exists が false。
func readTenantChoice(v string, t tenantInfo) (id int64, ok, exists bool) {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 1 {
		return 0, false, false
	}
	return id, true, t.Has(id)
}

// ---- 二重送信の防止 ----

// idemKeyPattern は、フォームに持たせる Idempotency-Key の形式（crypto/rand.Text の出力に、1 つの画面の中で
// フォームを見分ける後ろの部分を付けたもの。例 ABC…-state-vecgen-inactive）。
var idemKeyPattern = regexp.MustCompile(`^[A-Z2-7]{26}(-[a-z0-9-]{1,64})?$`)

// baseKey は Idempotency-Key の画面ごとの部分（フォームを見分ける後ろの部分を除く）を返す。
func baseKey(k string) string {
	b, _, _ := strings.Cut(k, "-")
	return b
}

// newIdemKey は、フォームを描くたびに作る Idempotency-Key を返す（もとの画面仕様 §1.5）。
// 同じフォームを送り直しても（応答を待たずに押し直した、タイムアウトの後に送り直した）、aka-provisioner は最初の結果を返す。
func newIdemKey() string { return rand.Text() }

// formIdemKey は、送られてきたフォームの Idempotency-Key（項目 name）を返す。形式が違えば新しく作る
// （aka-provisioner への書き込みには必ず付ける。段階 3-1 の W12）。
func formIdemKey(r *http.Request, name string) string {
	if k := r.PostForm.Get(name); idemKeyPattern.MatchString(k) {
		return k
	}
	return newIdemKey()
}

// sideKey は、1 つのフォームから両側に続けて書き込むときの、側ごとの Idempotency-Key（再送で同じ値になる）。
func sideKey(key string, side db.Side) string { return key + "-" + string(side) }

// keepIdemKey は、失敗の後にフォームを描き直すときの Idempotency-Key を返す。
// aka-provisioner に届かなかった・結果が分からない場合は、同じキーで送り直せるよう残す
// （aka-provisioner は 5xx を覚えないので、届いて失敗していても同じキーでやり直せる）。
// aka-provisioner が 4xx を返した場合は、その応答を覚えているので、新しいキーにする（入力を直して送ると内容が変わるため）。
func keepIdemKey(err error, key string) string {
	if key != "" && provisioner.ResultUnknown(err) {
		return key
	}
	return newIdemKey()
}

// ---- 監査ログ ----

// aka-ossbss の監査ログの操作の名前。
const (
	auditVecgenCreate     = audit.ActionVecgenCreate
	auditRadiusCreate     = audit.ActionRadiusCreate
	auditReregister       = "subscriber.reregister"
	auditVecgenUpdate     = "subscriber.vecgen.update"
	auditSourceUpdate     = "subscriber.radius.source"
	auditPolicyUpdate     = "subscriber.radius.rules"
	auditStateChange      = "subscriber.state"
	auditReinstate        = "subscriber.reinstate"
	auditSubscriberDelete = audit.ActionSubscriberDelete
	auditTenantMove       = "subscriber.tenant"
	auditCheck            = "subscriber.check"
	auditTenantCreate     = "tenant.create"
	auditTenantUpdate     = "tenant.update"
	auditTenantDelete     = "tenant.delete"
	auditAccountCreate    = "account.create"
	auditAccountUpdate    = "account.update"
	auditAccountDisable   = "account.disable"
	auditAccountEnable    = "account.enable"
	auditAccountDelete    = "account.delete"
)

// record は、成功した操作を aka-ossbss の監査ログ（PostgreSQL と日ごとのファイル）に残す。tenant は対象のテナント
// （テナントに属さないものは 0）。detail に秘密の値（Ki / OPc）を入れてはならない。
func (h *Handler) record(r *http.Request, action, target string, tenant int64, detail map[string]any) {
	h.recordEntry(r, db.AuditEntry{TenantID: tenant, Action: action, Target: target, Detail: detail})
}

// recordEntry は監査ログを書く（操作者・トレースID・接続元は要求から入れる）。
func (h *Handler) recordEntry(r *http.Request, e db.AuditEntry) {
	u := userFrom(r.Context())
	e.ActorEmail, e.ActorRole, e.TraceID, e.Remote = u.Email, u.Role, trace.From(r.Context()), h.ident.ClientIP(r)
	h.rec.Record(r.Context(), e)
}

// recordMove は、テナントが from から to に変わる操作（所属の付け替え、アカウントのテナントの変更）を監査ログに残す。
// 移動元と移動先が別のテナントなら、それぞれのテナントに 1 行ずつ書く（移動元のテナント管理者にも見えるように。
// テナントに属さない側には書かない）。両方とも属さないなら 1 行。
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
