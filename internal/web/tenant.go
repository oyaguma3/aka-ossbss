package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// テナントの画面（もとの画面仕様 §8）。作成・名前の変更・削除はシステム管理者だけ、外部の顧客番号は運用担当も設定できる（Z15）ので、
// 運用担当も一覧と詳細を見る。テナントの作成・変更・削除は、課金レコードの状態の事実を同じトランザクションで書く
// （db の CreateTenant など）。削除したテナントの ID は使い回さない。

// maxTenantNameLen はテナントの名前の文字数の上限（DB の CHECK 制約と同じ）。
const maxTenantNameLen = 100

// readTenantName はテナントの名前を読む（前後の空白を除く）。正しくなければ文を返す。
func readTenantName(r *http.Request) (string, string) {
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" || utf8.RuneCountInString(name) > maxTenantNameLen {
		return name, fmt.Sprintf("テナントの名前は 1〜%d 文字で入力してください。", maxTenantNameLen)
	}
	return name, ""
}

const msgTenantNameUsed = "そのテナント名は既に使われています。"

type tenantsData struct {
	Tenants []db.TenantSummary
	Name    string
	// NameError は作成のフォームの名前の誤り。
	NameError string
	Message   string
	Error     string
	// CanManage は作成のフォームとアカウントの件数を出すか。
	CanManage bool
}

// requireTenantView はテナントの一覧と詳細を見る権限を確かめる。
func (h *Handler) requireTenantView(w http.ResponseWriter, r *http.Request) bool {
	if can(r, authz.TenantManage) {
		return true
	}
	return h.requireOp(w, r, authz.ExternalIDUpdate)
}

func (h *Handler) tenantList(w http.ResponseWriter, r *http.Request) {
	if !h.requireTenantView(w, r) {
		return
	}
	d := tenantsData{}
	if r.URL.Query().Get("deleted") == "1" {
		// 名前はクエリで受け取らない（任意の文を出させない）。
		d.Message = "テナントを削除しました。"
	}
	h.renderTenants(w, r, http.StatusOK, d)
}

// renderTenants はテナントの一覧（htmx では一覧と作成のフォームの部分だけ）を返す。
func (h *Handler) renderTenants(w http.ResponseWriter, r *http.Request, status int, d tenantsData) {
	ctx, cancel := dbContext(r)
	list, err := h.db.TenantSummaries(ctx)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Tenants, d.CanManage = list, can(r, authz.TenantManage)
	h.renderPart(w, r, status, "tenants", "tenants-section", "テナント", d)
}

func (h *Handler) tenantCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.TenantManage) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	name, msg := readTenantName(r)
	if msg != "" {
		h.renderTenants(w, r, http.StatusBadRequest, tenantsData{Name: name, NameError: msg})
		return
	}
	ctx, cancel := dbContext(r)
	t, err := h.db.CreateTenant(ctx, name, meta(r))
	cancel()
	switch {
	case errors.Is(err, db.ErrConflict):
		h.renderTenants(w, r, http.StatusConflict, tenantsData{Name: name, NameError: msgTenantNameUsed})
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, auditTenantCreate, name, t.ID, map[string]any{"name": name})
	h.renderTenants(w, r, http.StatusOK, tenantsData{Message: "テナント「" + name + "」を作成しました。"})
}

type tenantData struct {
	Tenant db.TenantSummary
	// Name と NameError は名前の変更の入力と誤り。
	Name, NameError string
	// External と ExternalError は外部の顧客番号の入力と誤り。
	External, ExternalError string
	Message                 string
	Error                   string
	// CanManage は名前の変更と削除を出すか。CanExternal は外部の顧客番号の変更を出すか。
	CanManage, CanExternal bool
}

// InUse は、属するもの（所属の記録・アカウント）が残っているか。
func (d tenantData) InUse() bool {
	return d.Tenant.Subscribers > 0 || d.Tenant.Accounts > 0
}

// pathTenantID は URL のテナントの ID を返す。形式が違えば 404 を返して false。
func (h *Handler) pathTenantID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return 0, false
	}
	return id, true
}

const msgTenantNotFound = "テナントが見つかりません。他の操作で削除された可能性があります。"

// renderTenant はテナントの詳細（htmx では詳細の部分だけ）を返す。
func (h *Handler) renderTenant(w http.ResponseWriter, r *http.Request, id int64, status int, mutate func(*tenantData)) {
	ctx, cancel := dbContext(r)
	t, err := h.db.TenantSummary(ctx, id)
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, msgTenantNotFound, "/tenants", "テナントの一覧へ")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	d := tenantData{Tenant: t, Name: t.Name, External: t.ExternalCustomerID, CanManage: can(r, authz.TenantManage), CanExternal: can(r, authz.ExternalIDUpdate)}
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "tenant", "tenant-detail", "テナント "+t.Name, d)
}

func (h *Handler) tenant(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathTenantID(w, r)
	if !ok || !h.requireTenantView(w, r) {
		return
	}
	h.renderTenant(w, r, id, http.StatusOK, nil)
}

func (h *Handler) tenantRename(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathTenantID(w, r)
	if !ok || !h.requireOp(w, r, authz.TenantManage) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	name, msg := readTenantName(r)
	if msg != "" {
		h.renderTenant(w, r, id, http.StatusBadRequest, func(d *tenantData) { d.Name, d.NameError = name, msg })
		return
	}
	ctx, cancel := dbContext(r)
	before, err := h.db.RenameTenant(ctx, id, name, meta(r))
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, msgTenantNotFound, "/tenants", "テナントの一覧へ")
		return
	case errors.Is(err, db.ErrConflict):
		h.renderTenant(w, r, id, http.StatusConflict, func(d *tenantData) { d.Name, d.NameError = name, msgTenantNameUsed })
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	if before != name {
		h.record(r, auditTenantUpdate, name, id, map[string]any{"name": map[string]any{"from": before, "to": name}})
	}
	h.renderTenant(w, r, id, http.StatusOK, func(d *tenantData) { d.Message = "名前を変更しました。" })
}

// tenantExternal はテナントの外部の顧客番号を変える（運用担当・システム管理者）。状態の事実と監査ログに残す。
func (h *Handler) tenantExternal(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathTenantID(w, r)
	if !ok || !h.requireOp(w, r, authz.ExternalIDUpdate) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	value, msg := readExternalID(r)
	if msg != "" {
		h.renderTenant(w, r, id, http.StatusBadRequest, func(d *tenantData) { d.External, d.ExternalError = value, msg })
		return
	}
	ctx, cancel := dbContext(r)
	before, err := h.db.SetTenantExternalID(ctx, id, value, meta(r))
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, msgTenantNotFound, "/tenants", "テナントの一覧へ")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	if before != value {
		h.record(r, audit.ActionTenantExternal, formatID(id), id, map[string]any{"externalCustomerId": map[string]any{"from": before, "to": value}})
	}
	h.renderTenant(w, r, id, http.StatusOK, func(d *tenantData) { d.Message = "外部の顧客番号を変更しました。" })
}

func (h *Handler) tenantDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathTenantID(w, r)
	if !ok || !h.requireOp(w, r, authz.TenantManage) {
		return
	}
	ctx, cancel := dbContext(r)
	t, err := h.db.TenantSummary(ctx, id)
	if err == nil {
		err = h.db.DeleteTenant(ctx, id, meta(r))
	}
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, msgTenantNotFound, "/tenants", "テナントの一覧へ")
		return
	case errors.Is(err, db.ErrInUse):
		h.renderTenant(w, r, id, http.StatusConflict, func(d *tenantData) {
			d.Error = fmt.Sprintf("このテナントには加入者 %d 件、アカウント %d 件が属しています。"+
				"先に加入者を物理削除するか付け替え、アカウントを移すか削除してから削除してください。", d.Tenant.Subscribers, d.Tenant.Accounts)
		})
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, auditTenantDelete, t.Name, id, map[string]any{"name": t.Name})
	seeOther(w, r, "/tenants?deleted=1")
}
