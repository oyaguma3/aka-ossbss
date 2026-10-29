package web

import (
	"net/http"
	"regexp"
	"strconv"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 監査ログの画面（設計概要 §14。段階 3-1 の W10）。aka-ossbss の監査ログはテナントの範囲で見せる（テナント管理者は自分の
// テナントの分だけ）。aka-provisioner と下流（aka-vecgen・aka-radius）の監査ログは aka-provisioner を通して読み、テナントで
// 絞れないので、運用担当とシステム管理者が見る。

// auditPerPage は監査ログの 1 回に読み込む件数。
const auditPerPage = 50

// streamIDPattern は provisioner と下流の監査ログのエントリID（Valkey の Stream の ID）の形式。
var streamIDPattern = regexp.MustCompile(`^[0-9]{1,20}-[0-9]{1,20}$`)

// auditTabs は監査ログの種類のタブ（templates/partials/common.html の audit-tabs）。
type auditTabs struct {
	// Current は表示中のタブ（ossbss / provisioner / vecgen / radius）。
	Current string
	// Downstream は aka-provisioner と下流のタブを出すか（運用担当・システム管理者）。
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

// auditList は aka-ossbss の監査ログの画面（テナント管理者・運用担当・システム管理者）。
func (h *Handler) auditList(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.AuditView) {
		return
	}
	q := r.URL.Query()
	d := auditData{Tabs: auditTabsFor(r, "ossbss"), Action: q.Get("action"), Actions: auditActions, More: q.Get("before") != ""}
	status := http.StatusOK
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Tenants = t
	f, ok := readTenantFilter(r, t, true)
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

// downstreamAuditData は aka-provisioner と下流の監査ログの画面に渡す値。
type downstreamAuditData struct {
	Tabs   auditTabs
	Source string
	Title  string
	Rows   []provisioner.AuditLogEntry
	Next   string
	Error  string
	More   bool
}

// auditSources は aka-provisioner と下流の監査ログの元と、画面の名前。
var auditSources = map[string]string{
	string(provisioner.AuditProvisioner): "aka-provisioner",
	string(provisioner.AuditVecgen):      "aka-vecgen",
	string(provisioner.AuditRadius):      "aka-radius",
}

// downstreamAudit は aka-provisioner と下流の監査ログ（運用担当・システム管理者）。
func (h *Handler) downstreamAudit(w http.ResponseWriter, r *http.Request) {
	src := r.PathValue("source")
	name, ok := auditSources[src]
	if !ok {
		h.notFound(w, r)
		return
	}
	if !h.requireOp(w, r, authz.DownstreamAuditView) {
		return
	}
	before := r.URL.Query().Get("before")
	d := downstreamAuditData{Tabs: auditTabsFor(r, src), Source: src, Title: name, More: before != ""}
	status := http.StatusOK
	if before != "" && !streamIDPattern.MatchString(before) {
		status, d.Error = http.StatusBadRequest, "続きの位置の指定が正しくありません。"
	} else if l, err := h.pv.AuditLogs(r.Context(), provisioner.AuditSource(src), provisioner.AuditLogParams{Before: before, Limit: auditPerPage}); err != nil {
		f := h.apiError(r, err, "")
		h.log.Warn("list "+src+" audit logs", "error", err)
		status, d.Error = f.Status, f.Message
	} else {
		d.Rows, d.Next = l.Items, l.NextBefore
	}
	if d.More && isHTMX(r) {
		h.renderBlock(w, r, status, "audit_downstream", "audit-more", d)
		return
	}
	h.render(w, r, status, "audit_downstream", "監査ログ（"+name+"）", d)
}
