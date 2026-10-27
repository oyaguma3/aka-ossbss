package web

import (
	"net/http"
	"regexp"
	"slices"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// provisioner の操作の記録の画面（画面仕様 §10）。2 つのノードにまたがる加入者の登録・変更・削除の、手順ごとの状態を見る。
// 閲覧は運用担当・システム管理者、やり直し（retry）と閉じる（dismiss）はシステム管理者だけ（設計概要 §6.2）。
// 既存の BFF の operation.go を元にした。

// operationsPerPage は操作の記録の一覧の件数の上限。
const operationsPerPage = 100

// operationIDPattern は操作の ID（UUID）の形式。
var operationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// operationStatuses は一覧で絞り込める状態（未完了のもの）。
var operationStatuses = []string{provisioner.OpRunning, provisioner.OpRetrying, provisioner.OpFailed}

type operationListData struct {
	// Status は絞り込みの状態（空ならすべて）。
	Status   string
	Statuses []string
	Items    []provisioner.Operation
	Total    int
	// Tenants は IMSI の所属のテナントの名前（IMSI → 名前）。
	Tenants map[string]string
	Error   string
}

// tenantNamesOf は、IMSI の一覧の所属のテナントの名前を引く（引けなければ空のまま）。
func (h *Handler) tenantNamesOf(r *http.Request, imsis []string) map[string]string {
	out := map[string]string{}
	t, err := h.tenants(r)
	if err != nil {
		h.log.Error("tenants", "error", err)
		return out
	}
	ctx, cancel := dbContext(r)
	assigned, err := h.db.SubscriberTenants(ctx, imsis)
	cancel()
	if err != nil {
		h.log.Error("subscriber tenants", "error", err)
		return out
	}
	for _, imsi := range imsis {
		out[imsi] = t.Name(assigned[imsi])
	}
	return out
}

func (h *Handler) operations(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ProvisionerView) {
		return
	}
	d := operationListData{Status: r.URL.Query().Get("status"), Statuses: operationStatuses}
	if d.Status != "" && !slices.Contains(operationStatuses, d.Status) {
		d.Error = "状態の指定が正しくありません。"
		h.render(w, r, http.StatusBadRequest, "operations", "操作の記録", d)
		return
	}
	list, err := h.pv.ListOperations(r.Context(), d.Status, operationsPerPage)
	if err != nil {
		f := h.apiError(r, err, "")
		h.log.Warn("list operations", "error", err)
		d.Error = f.Message
		h.render(w, r, f.Status, "operations", "操作の記録", d)
		return
	}
	d.Items, d.Total = list.Items, list.Total
	imsis := make([]string, len(list.Items))
	for i, op := range list.Items {
		imsis[i] = op.IMSI
	}
	d.Tenants = h.tenantNamesOf(r, imsis)
	h.render(w, r, http.StatusOK, "operations", "操作の記録", d)
}

type operationData struct {
	Op provisioner.Operation
	// Tenant は IMSI の所属のテナントの名前。
	Tenant string
	// CanAct は、やり直し・閉じるができるか（システム管理者）。
	CanAct bool
	// RetryKey と DismissKey は、やり直しと閉じるのフォームの Idempotency-Key。
	RetryKey, DismissKey string
	Message              string
	Error                string
}

// CanRetry は、この操作をやり直せるか（failed だけ。鍵の変更が反映されたか分からない変更は除く）。
func (d operationData) CanRetry() bool {
	return d.Op.Status == provisioner.OpFailed && !d.KeyChangeUnknown()
}

// CanDismiss は、この操作を閉じられるか（retrying / failed）。
func (d operationData) CanDismiss() bool {
	return d.Op.Status == provisioner.OpRetrying || d.Op.Status == provisioner.OpFailed
}

// KeyChangeUnknown は、鍵の変更が反映されたかどうか分からない変更か（provisioner は自動でも手でもやり直さない）。
// 変更の記録で、鍵の変更の手順が済んでも失敗してもいない（未実施のまま要求が落ちた）まま failed になったもの。
func (d operationData) KeyChangeUnknown() bool {
	if d.Op.Kind != "subscriber.update" || d.Op.Status != provisioner.OpFailed {
		return false
	}
	i := slices.IndexFunc(d.Op.Steps, func(s provisioner.OperationStep) bool { return s.Name == "subscriber.update" })
	return i >= 0 && d.Op.Steps[i].State == "pending"
}

// pathOperationID は URL の操作の ID を返す。形式が違えば 404 を返して false。
func (h *Handler) pathOperationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !operationIDPattern.MatchString(id) {
		h.notFound(w, r)
		return "", false
	}
	return id, true
}

// operationNotFound は操作の記録が見つからないときの説明。
const operationNotFound = "操作の記録が見つかりません。完了した操作の記録は 7 日で消えます。"

// renderOperation は操作の記録の詳細を返す（htmx では詳細の部分だけ）。
func (h *Handler) renderOperation(w http.ResponseWriter, r *http.Request, id string, status int, mutate func(*operationData)) {
	op, err := h.pv.GetOperation(r.Context(), id)
	if err != nil {
		f := h.apiError(r, err, operationNotFound)
		if f.Status != http.StatusNotFound {
			h.log.Warn("get operation", "error", err)
		}
		h.renderFailure(w, r, f, "/operations", "操作の記録の一覧へ")
		return
	}
	d := operationData{Op: op, Tenant: h.tenantNamesOf(r, []string{op.IMSI})[op.IMSI], CanAct: can(r, authz.OperationManage),
		RetryKey: newIdemKey(), DismissKey: newIdemKey()}
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "operation", "operation-detail", "操作の記録", d)
}

func (h *Handler) operation(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathOperationID(w, r)
	if !ok || !h.requireOp(w, r, authz.ProvisionerView) {
		return
	}
	h.renderOperation(w, r, id, http.StatusOK, nil)
}

// operationRetry は failed の操作の続きを、その場で 1 回行う（システム管理者だけ）。
func (h *Handler) operationRetry(w http.ResponseWriter, r *http.Request) {
	h.operationAction(w, r, auditOperationRetry)
}

// operationDismiss は retrying / failed の操作を、手で直した後に閉じる（下流には何もしない。システム管理者だけ）。
func (h *Handler) operationDismiss(w http.ResponseWriter, r *http.Request) {
	h.operationAction(w, r, auditOperationDismiss)
}

func (h *Handler) operationAction(w http.ResponseWriter, r *http.Request, action string) {
	id, ok := h.pathOperationID(w, r)
	if !ok || !h.requireOp(w, r, authz.OperationManage) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	idem := formIdemKey(r, "idem")
	var op provisioner.Operation
	var err error
	if action == auditOperationRetry {
		op, err = h.pv.RetryOperation(r.Context(), id, idem)
	} else {
		op, err = h.pv.DismissOperation(r.Context(), id, idem)
	}
	if err != nil {
		f := h.apiError(r, err, operationNotFound)
		h.log.Warn("operation "+action, "operation_id", id, "error", err)
		h.renderOperation(w, r, id, f.Status, func(d *operationData) {
			d.Error = f.Message
			if action == auditOperationRetry {
				d.RetryKey = keepIdemKey(err, idem)
			} else {
				d.DismissKey = keepIdemKey(err, idem)
			}
		})
		return
	}
	// 対象のテナントは IMSI の所属（テナント管理者が自分のテナントの加入者の操作として見られるように）。
	ctx, cancel := dbContext(r)
	tenant, terr := h.db.SubscriberTenant(ctx, op.IMSI)
	cancel()
	if terr != nil {
		h.log.Error("subscriber tenant", "error", terr)
	}
	h.record(r, action, id, tenant, map[string]any{"imsi": op.IMSI, "kind": op.Kind, "status": op.Status})
	h.renderOperation(w, r, id, http.StatusOK, func(d *operationData) {
		switch {
		case action == auditOperationDismiss:
			d.Message = "操作を閉じました。"
		case op.Status == provisioner.OpRetrying:
			d.Error = "やり直しましたが、また失敗しました。provisioner が自動でのやり直しを再開します（24 時間）。"
		default:
			d.Message = "やり直しました。操作は「" + opStatusLabel(op.Status) + "」になりました。"
		}
	})
}
