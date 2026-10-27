package web

import (
	"net/http"
	"regexp"
	"strconv"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 監査ログの画面（画面仕様 §11）。本BFF の監査ログはテナントの範囲で見せる（テナント管理者は自分のテナントの分だけ。
// 設計概要 §13）。provisioner と下流の監査ログ（provisioner の中継）はテナントで絞れないので、システム管理者だけが見る。

// auditPerPage は監査ログの 1 回に読み込む件数。
const auditPerPage = 50

// streamIDPattern は provisioner と下流の監査ログのエントリID（Valkey の Stream の ID）の形式。
var streamIDPattern = regexp.MustCompile(`^[0-9]{1,20}-[0-9]{1,20}$`)

// auditTabs は監査ログの種類のタブ（templates/partials/common.html の audit-tabs）。
type auditTabs struct {
	// Current は表示中のタブ（bff / provisioner / prov / aka）。
	Current string
	// Downstream は provisioner と下流のタブを出すか（システム管理者）。
	Downstream bool
}

func auditTabsFor(r *http.Request, current string) auditTabs {
	return auditTabs{Current: current, Downstream: can(r, authz.DownstreamAuditView)}
}

type auditData struct {
	Tabs    auditTabs
	Filter  tenantFilter
	Tenants tenantInfo
	// Action は操作の種類の絞り込み（空ならすべて）。Actions は選択肢。
	Action  string
	Actions []struct{ Action, Label string }
	Rows    []db.AuditRecord
	// Next は続きの位置（最後の行の ID。なければ 0）。
	Next  int64
	Error string
	// More は「さらに古いものを表示」で続きを読み込んだ応答か。
	More bool
}

// audit は本BFF の監査ログの画面（テナント管理者・運用担当・システム管理者）。
func (h *Handler) audit(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.AuditView) {
		return
	}
	q := r.URL.Query()
	d := auditData{Tabs: auditTabsFor(r, "bff"), Action: q.Get("action"), Actions: auditActions, More: q.Get("before") != ""}
	status := http.StatusOK
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Tenants = t
	f, ok := readTenantFilter(r, t)
	d.Filter = f
	before, berr := strconv.ParseInt(q.Get("before"), 10, 64)
	switch {
	case !ok:
		status, d.Error = http.StatusBadRequest, "テナントの指定が正しくありません。"
	case d.Action != "" && !knownAction(d.Action):
		status, d.Error = http.StatusBadRequest, "操作の種類の指定が正しくありません。"
	case d.More && (berr != nil || before < 1):
		status, d.Error = http.StatusBadRequest, "続きの位置の指定が正しくありません。"
	default:
		// テナント管理者は readTenantFilter で自分のテナントに固定される。
		ctx, cancel := dbContext(r)
		rows, err := h.db.ListAudit(ctx, db.AuditQuery{AllTenants: f.All, NoTenant: f.None, TenantID: f.Tenant,
			Action: d.Action, Before: before, Limit: auditPerPage + 1})
		cancel()
		if err != nil {
			h.log.Error("list audit", "error", err)
			status, d.Error = http.StatusServiceUnavailable, "データベースに接続できません。しばらくしてから開き直してください。"
			break
		}
		if len(rows) > auditPerPage {
			rows = rows[:auditPerPage]
			d.Next = rows[len(rows)-1].ID
		}
		d.Rows = rows
	}
	if d.More && isHTMX(r) {
		h.renderBlock(w, r, status, "audit", "audit-more", d)
		return
	}
	h.render(w, r, status, "audit", "監査ログ", d)
}

// downstreamAuditData は provisioner と下流の監査ログの画面に渡す値（Rows は種類ごとの型）。
type downstreamAuditData[T any] struct {
	Tabs  auditTabs
	Rows  []T
	Next  string
	Error string
	More  bool
}

// downstreamAudit は provisioner と下流の監査ログの画面の共通の処理（システム管理者だけ）。
// list は provisioner の監査ログの読み出し、next は続きの位置を返す。
func downstreamAudit[T, L any](h *Handler, w http.ResponseWriter, r *http.Request, tab, page, title string,
	list func(*http.Request, provisioner.AuditLogParams) (L, error), items func(L) ([]T, string)) {
	if !h.requireOp(w, r, authz.DownstreamAuditView) {
		return
	}
	before := r.URL.Query().Get("before")
	d := downstreamAuditData[T]{Tabs: auditTabsFor(r, tab), More: before != ""}
	status := http.StatusOK
	if before != "" && !streamIDPattern.MatchString(before) {
		status, d.Error = http.StatusBadRequest, "続きの位置の指定が正しくありません。"
	} else if l, err := list(r, provisioner.AuditLogParams{Before: before, Limit: auditPerPage}); err != nil {
		f := h.apiError(r, err, "")
		h.log.Warn("list "+tab+" audit logs", "error", err)
		status, d.Error = f.Status, f.Message
	} else {
		d.Rows, d.Next = items(l)
	}
	if d.More && isHTMX(r) {
		h.renderBlock(w, r, status, page, "audit-more", d)
		return
	}
	h.render(w, r, status, page, title, d)
}

// pvAudit は provisioner 自身の監査ログ。
func (h *Handler) pvAudit(w http.ResponseWriter, r *http.Request) {
	downstreamAudit(h, w, r, "provisioner", "audit_provisioner", "監査ログ（provisioner）",
		func(r *http.Request, p provisioner.AuditLogParams) (provisioner.AuditLogList, error) {
			return h.pv.ListAuditLogs(r.Context(), p)
		},
		func(l provisioner.AuditLogList) ([]provisioner.AuditLogEntry, string) { return l.Items, l.NextBefore })
}

// provAudit は provisioner が中継する、本PoCの provisioning-api の監査ログ。
func (h *Handler) provAudit(w http.ResponseWriter, r *http.Request) {
	downstreamAudit(h, w, r, "prov", "audit_prov", "監査ログ（provisioning-api）",
		func(r *http.Request, p provisioner.AuditLogParams) (provisioner.ProvAuditLogList, error) {
			return h.pv.ListProvAuditLogs(r.Context(), p)
		},
		func(l provisioner.ProvAuditLogList) ([]provisioner.ProvAuditLogEntry, string) {
			return l.Items, l.NextBefore
		})
}

// akaAudit は provisioner が中継する、aka-vecgen の監査ログ。
func (h *Handler) akaAudit(w http.ResponseWriter, r *http.Request) {
	downstreamAudit(h, w, r, "aka", "audit_aka", "監査ログ（aka-vecgen）",
		func(r *http.Request, p provisioner.AuditLogParams) (provisioner.AkaAuditLogList, error) {
			return h.pv.ListAkaAuditLogs(r.Context(), p)
		},
		func(l provisioner.AkaAuditLogList) ([]provisioner.AkaAuditLogEntry, string) {
			return l.Items, l.NextBefore
		})
}
