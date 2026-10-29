package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// アカウントの画面（画面仕様 §9）。システム管理者はすべて、テナント管理者は自分のテナントのテナントユーザーだけを扱う
// （authz.CanManageAccount。設計概要 §15 の 11）。最初のシステム管理者と自分自身のアカウントは、削除・無効化・
// 役割やテナントの変更を断る（§17.2 の 3、§17.5 の 4）。

const (
	msgEmailFormat       = "メールアドレスは、英数字と . _ - と @ の 64 文字までで入力してください（+ などは使えません）。"
	msgEmailUsed         = "そのメールアドレスのアカウントは既にあります。"
	msgAccountNotFound   = "アカウントが見つかりません。他の操作で削除された可能性があります。"
	msgAccountChanged    = "アカウントは他の操作で変更されました。画面を開き直してください。"
	msgInitialAdminFixed = "最初のシステム管理者は変更・削除できません（起動のたびにシステム管理者・有効に戻ります）。"
	msgSelfFixed         = "自分のアカウントは変更・削除できません。"
)

// accountRow はアカウントの一覧の 1 行。
type accountRow struct {
	db.Account
	// Manageable は、利用者がこのアカウントを開いて操作できるか（詳細へのリンクを付ける）。
	Manageable bool
	// Initial は最初のシステム管理者、Self は利用者自身か。
	Initial, Self bool
}

// accountForm はアカウントの作成の入力。
type accountForm struct {
	Email, Role, Tenant string
	Errors              fieldErrors
}

type accountsData struct {
	// Admin は、システム管理者の画面か（テナントの絞り込み、役割とテナントの選択を出す）。
	Admin   bool
	Filter  tenantFilter
	Tenants tenantInfo
	Rows    []accountRow
	Roles   []authz.Role
	Form    accountForm
	Message string
	Error   string
}

// accountFilter は利用者と絞り込みから、DB のアカウントの絞り込みを作る。
func accountFilter(f tenantFilter) db.AccountFilter {
	return db.AccountFilter{All: f.All, NoTenant: f.None, TenantID: f.Tenant}
}

// loadAccounts はアカウントの一覧を集める。
func (h *Handler) loadAccounts(r *http.Request, d *accountsData) (int, string) {
	t, err := h.tenants(r)
	if err != nil {
		h.log.Error("tenants", "error", err)
		return http.StatusServiceUnavailable, "データベースに接続できません。しばらくしてから開き直してください。"
	}
	d.Tenants = t
	f, ok := readTenantFilter(r, t, true)
	d.Filter = f
	if !ok {
		return http.StatusBadRequest, "テナントの指定が正しくありません。"
	}
	ctx, cancel := dbContext(r)
	list, err := h.db.Accounts(ctx, accountFilter(f))
	cancel()
	if err != nil {
		h.log.Error("accounts", "error", err)
		return http.StatusServiceUnavailable, "データベースに接続できません。しばらくしてから開き直してください。"
	}
	u := userFrom(r.Context())
	for _, a := range list {
		d.Rows = append(d.Rows, accountRow{Account: a, Manageable: authz.CanManageAccount(u.Actor(), a.Role, a.TenantID),
			Initial: a.Email == h.initialAdmin, Self: a.Email == u.Email})
	}
	return http.StatusOK, ""
}

func newAccountsData(r *http.Request) accountsData {
	admin := userFrom(r.Context()).Role == authz.SystemAdmin
	return accountsData{Admin: admin, Roles: authz.Roles, Form: accountForm{Role: string(authz.TenantUser), Errors: fieldErrors{}}}
}

func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.AccountManage) {
		return
	}
	d := newAccountsData(r)
	status, msg := h.loadAccounts(r, &d)
	d.Error = msg
	if r.URL.Query().Get("deleted") == "1" {
		d.Message = "アカウントを削除しました。"
	}
	h.render(w, r, status, "accounts", "アカウント", d)
}

// renderAccounts は一覧の画面（htmx では一覧と作成のフォームの部分だけ）を返す。
func (h *Handler) renderAccounts(w http.ResponseWriter, r *http.Request, status int, d accountsData) {
	h.renderPart(w, r, status, "accounts", "accounts-section", "アカウント", d)
}

// readAccountRole は、システム管理者のフォームの役割とテナントを読む。正しくなければ誤りを errs に書く。
func readAccountRole(role, tenant string, t tenantInfo, errs fieldErrors) (authz.Role, int64) {
	r := authz.Role(role)
	errs.check(r.Valid(), "role", "役割を選んでください。")
	if !r.TenantScoped() {
		// 運用担当・システム管理者はテナントに属さない（選んでいても使わない）。
		return r, authz.NoTenant
	}
	id, chosen, exists := readTenantChoice(tenant, t)
	errs.check(chosen && id != authz.NoTenant, "tenant", "テナントユーザーとテナント管理者は、テナントを選んでください。")
	errs.check(!chosen || id == authz.NoTenant || exists, "tenant", "選んだテナントはありません。画面を開き直してください。")
	return r, id
}

func (h *Handler) accountCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.AccountManage) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	u := userFrom(r.Context())
	pf := r.PostForm
	f := accountForm{Email: authz.NormalizeEmail(pf.Get("email")), Role: pf.Get("role"), Tenant: pf.Get("tenant"), Errors: fieldErrors{}}
	fail := func(status int, msg string) {
		d := newAccountsData(r)
		_, loadMsg := h.loadAccounts(r, &d)
		if msg == "" {
			msg = loadMsg
		}
		d.Form, d.Error = f, msg
		h.renderAccounts(w, r, status, d)
	}
	f.Errors.check(authz.ValidEmail(f.Email), "email", msgEmailFormat)
	// テナント管理者は、自分のテナントのテナントユーザーだけを作る。
	role, tenant := authz.TenantUser, u.TenantID
	if u.Role == authz.SystemAdmin {
		t, err := h.tenants(r)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		role, tenant = readAccountRole(f.Role, f.Tenant, t, f.Errors)
	}
	if len(f.Errors) > 0 {
		fail(http.StatusBadRequest, "入力を確かめてください。")
		return
	}
	if !authz.CanManageAccount(u.Actor(), role, tenant) {
		h.forbidden(w, r)
		return
	}
	ctx, cancel := dbContext(r)
	_, err := h.db.CreateAccount(ctx, f.Email, role, tenant, u.Email)
	cancel()
	switch {
	case errors.Is(err, db.ErrConflict):
		f.Errors["email"] = msgEmailUsed
		fail(http.StatusConflict, msgEmailUsed)
		return
	case errors.Is(err, db.ErrNotFound):
		fail(http.StatusConflict, "選んだテナントはありません。画面を開き直してください。")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, auditAccountCreate, f.Email, tenant, map[string]any{"role": string(role)})
	d := newAccountsData(r)
	status, loadMsg := h.loadAccounts(r, &d)
	d.Error = loadMsg
	d.Message = "アカウント " + f.Email + " を作成しました。Cloudflare Access のポリシーにも、このメールアドレス（またはドメイン）を加えてください。"
	h.renderAccounts(w, r, status, d)
}

// ---- 詳細と変更 ----

type accountData struct {
	Account db.Account
	// Admin は役割・テナントを変えられるか（システム管理者）。
	Admin bool
	// Fixed は、変更・削除を断る理由（最初のシステム管理者、自分自身。空なら操作できる）。
	Fixed   string
	Tenants tenantInfo
	Roles   []authz.Role
	// Role と Tenant は変更の入力（誤りのときに戻す）。
	Role, Tenant string
	Errors       fieldErrors
	Message      string
	Error        string
}

// pathAccountID は URL のアカウントの ID を返す。形式が違えば 404 を返して false。
func (h *Handler) pathAccountID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return 0, false
	}
	return id, true
}

// loadAccount はアカウントを読み、利用者が扱えるかを確かめる（要求ごとに必ず通す）。
// テナント管理者には、他のテナントのアカウントは 404、自分のテナントのテナントユーザー以外は 403 を返す。
func (h *Handler) loadAccount(w http.ResponseWriter, r *http.Request, id int64) (db.Account, bool) {
	if !h.requireOp(w, r, authz.AccountManage) {
		return db.Account{}, false
	}
	ctx, cancel := dbContext(r)
	a, err := h.db.AccountByID(ctx, id)
	cancel()
	u := userFrom(r.Context())
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && u.Role.TenantScoped() && a.TenantID != u.TenantID):
		h.renderErrorLink(w, r, http.StatusNotFound, msgAccountNotFound, "/accounts", "アカウントの一覧へ")
		return db.Account{}, false
	case err != nil:
		h.dbUnavailable(w, r, err)
		return db.Account{}, false
	case !authz.CanManageAccount(u.Actor(), a.Role, a.TenantID):
		h.forbidden(w, r)
		return db.Account{}, false
	}
	return a, true
}

// fixedReason は、アカウントの変更・削除を断る理由（なければ空）。
func (h *Handler) fixedReason(r *http.Request, a db.Account) string {
	switch {
	case a.Email == h.initialAdmin:
		return msgInitialAdminFixed
	case a.Email == userFrom(r.Context()).Email:
		return msgSelfFixed
	}
	return ""
}

// renderAccount はアカウントの詳細（htmx では詳細の部分だけ）を返す。
func (h *Handler) renderAccount(w http.ResponseWriter, r *http.Request, id int64, status int, mutate func(*accountData)) {
	a, ok := h.loadAccount(w, r, id)
	if !ok {
		return
	}
	d := accountData{Account: a, Admin: userFrom(r.Context()).Role == authz.SystemAdmin, Fixed: h.fixedReason(r, a), Roles: authz.Roles,
		Role: string(a.Role), Tenant: strconv.FormatInt(a.TenantID, 10), Errors: fieldErrors{}}
	if a.TenantID == authz.NoTenant {
		d.Tenant = filterNone
	}
	if d.Admin {
		t, err := h.tenants(r)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		d.Tenants = t
	}
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "account", "account-detail", "アカウント "+a.Email, d)
}

func (h *Handler) account(w http.ResponseWriter, r *http.Request) {
	if id, ok := h.pathAccountID(w, r); ok {
		h.renderAccount(w, r, id, http.StatusOK, nil)
	}
}

// accountAction は変更・無効化・有効化・削除の共通の前処理。アカウントを読んで権限を確かめ、画面を開いた時点の
// 役割・テナント（from_role、from_tenant）が今と同じことを確かめる。断ったら画面を返して false。
func (h *Handler) accountAction(w http.ResponseWriter, r *http.Request) (db.Account, db.AccountState, bool) {
	id, ok := h.pathAccountID(w, r)
	if !ok {
		return db.Account{}, db.AccountState{}, false
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return db.Account{}, db.AccountState{}, false
	}
	a, ok := h.loadAccount(w, r, id)
	if !ok {
		return db.Account{}, db.AccountState{}, false
	}
	cur := db.AccountState{Role: a.Role, TenantID: a.TenantID}
	fromTenant, _ := strconv.ParseInt(r.PostForm.Get("from_tenant"), 10, 64)
	if r.PostForm.Get("from_role") != string(a.Role) || fromTenant != a.TenantID {
		h.renderAccount(w, r, id, http.StatusConflict, func(d *accountData) { d.Error = msgAccountChanged })
		return db.Account{}, db.AccountState{}, false
	}
	if reason := h.fixedReason(r, a); reason != "" {
		h.renderAccount(w, r, id, http.StatusForbidden, func(d *accountData) { d.Error = reason })
		return db.Account{}, db.AccountState{}, false
	}
	return a, cur, true
}

// accountUpdate は役割とテナントを変える（システム管理者だけ）。
func (h *Handler) accountUpdate(w http.ResponseWriter, r *http.Request) {
	a, cur, ok := h.accountAction(w, r)
	if !ok {
		return
	}
	u := userFrom(r.Context())
	if u.Role != authz.SystemAdmin {
		h.forbidden(w, r)
		return
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	errs := fieldErrors{}
	role, tenant := readAccountRole(r.PostForm.Get("role"), r.PostForm.Get("tenant"), t, errs)
	if len(errs) > 0 {
		h.renderAccount(w, r, a.ID, http.StatusBadRequest, func(d *accountData) {
			d.Errors, d.Error, d.Role, d.Tenant = errs, "入力を確かめてください。", r.PostForm.Get("role"), r.PostForm.Get("tenant")
		})
		return
	}
	next := db.AccountState{Role: role, TenantID: tenant}
	if !authz.CanManageAccount(u.Actor(), role, tenant) {
		h.forbidden(w, r)
		return
	}
	if next == cur {
		h.renderAccount(w, r, a.ID, http.StatusOK, func(d *accountData) { d.Message = "変更はありません。" })
		return
	}
	ctx, cancel := dbContext(r)
	err = h.db.UpdateAccount(ctx, a.ID, cur, next)
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderAccount(w, r, a.ID, http.StatusConflict, func(d *accountData) { d.Error = msgAccountChanged })
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.recordMove(r, auditAccountUpdate, a.Email, cur.TenantID, next.TenantID, map[string]any{
		"role":   map[string]any{"from": string(cur.Role), "to": string(next.Role)},
		"tenant": map[string]any{"from": t.Name(cur.TenantID), "to": t.Name(next.TenantID)},
	})
	h.renderAccount(w, r, a.ID, http.StatusOK, func(d *accountData) {
		d.Message = "役割とテナントを変更しました。次の要求から効きます。"
	})
}

func (h *Handler) accountDisable(w http.ResponseWriter, r *http.Request) {
	h.setAccountDisabled(w, r, true)
}
func (h *Handler) accountEnable(w http.ResponseWriter, r *http.Request) {
	h.setAccountDisabled(w, r, false)
}

// setAccountDisabled はアカウントを無効・有効にする。無効にしたアカウントは、次の要求から 403 になる（設計概要 §5.3）。
func (h *Handler) setAccountDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	a, cur, ok := h.accountAction(w, r)
	if !ok {
		return
	}
	ctx, cancel := dbContext(r)
	err := h.db.SetAccountDisabled(ctx, a.ID, cur, disabled)
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderAccount(w, r, a.ID, http.StatusConflict, func(d *accountData) { d.Error = msgAccountChanged })
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	action, msg := auditAccountEnable, "有効にしました。"
	if disabled {
		action, msg = auditAccountDisable, "無効にしました。次の要求から、この利用者は画面を使えなくなります。"
	}
	h.record(r, action, a.Email, a.TenantID, nil)
	h.renderAccount(w, r, a.ID, http.StatusOK, func(d *accountData) { d.Message = msg })
}

// accountDelete はアカウントを削除する。
func (h *Handler) accountDelete(w http.ResponseWriter, r *http.Request) {
	a, cur, ok := h.accountAction(w, r)
	if !ok {
		return
	}
	ctx, cancel := dbContext(r)
	err := h.db.DeleteAccount(ctx, a.ID, cur)
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusConflict, msgAccountChanged, "/accounts", "アカウントの一覧へ")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, auditAccountDelete, a.Email, a.TenantID, map[string]any{"role": string(a.Role)})
	seeOther(w, r, "/accounts?deleted=1")
}
