package web

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// RADIUSクライアントの画面（画面仕様 §4）。provisioner（本PoCの Provisioning API の中継）の全件を取り、
// 本BFF の所属の記録で絞る（設計概要 §7.1）。

// RADIUSクライアントの入力の誤りの説明。
const (
	msgClientIP     = "IP アドレスは IPv4 のドット区切り（例 192.168.10.1。各数の先頭に 0 を付けない）で入力してください。"
	msgClientSecret = "共有シークレットは、空白を含まない英数字・記号（印字可能な ASCII）の 1〜128 文字で入力してください。"
	msgClientName   = "名前は英数字と _ - の 1〜64 文字で入力してください。"
	msgClientVendor = "ベンダーは英数字・空白・- の 64 文字までで入力してください。"
)

// clientForm は RADIUSクライアントの入力。共有シークレットは画面に戻さない。
type clientForm struct {
	IP, Secret, Name, Vendor string
	// Tenant は所属のテナントの選択（登録のときの運用担当・システム管理者だけ）。
	Tenant  string
	IdemKey string
	Errors  fieldErrors
}

// readClientForm はフォームの入力を読み、確かめる。requireSecret が false なら、共有シークレットは空でもよい（変更しない）。
func readClientForm(r *http.Request, requireSecret bool) clientForm {
	pf := r.PostForm
	f := clientForm{
		IP: strings.TrimSpace(pf.Get("ip")), Secret: strings.TrimSpace(pf.Get("secret")),
		Name: strings.TrimSpace(pf.Get("name")), Vendor: strings.TrimSpace(pf.Get("vendor")),
		Tenant: pf.Get("tenant"), IdemKey: formIdemKey(r, "idem"), Errors: fieldErrors{},
	}
	f.Errors.check(validIPv4(f.IP), "ip", msgClientIP)
	f.Errors.check((!requireSecret && f.Secret == "") || secretPattern.MatchString(f.Secret), "secret", msgClientSecret)
	f.Errors.check(clientNamePattern.MatchString(f.Name), "name", msgClientName)
	f.Errors.check(vendorPattern.MatchString(f.Vendor), "vendor", msgClientVendor)
	return f
}

// ---- 一覧と登録 ----

// clientRow は一覧の 1 行。
type clientRow struct {
	ID int64
	// Client は provisioner の RADIUSクライアント（Missing のときは空）。
	Client provisioner.RADIUSClient
	// Missing は、所属の記録があるのに provisioner にないこと（下流にない）。
	Missing bool
	Tenant  int64
}

type clientsData struct {
	// Staff はテナントの列を出すか（C・D）。CanCreate は登録のフォームを出すか（B・C・D）。
	Staff, CanCreate bool
	// ChooseTenant は登録で所属のテナントを選ぶか（C・D。B は自分のテナントに属する）。
	ChooseTenant bool
	Filter       tenantFilter
	Tenants      tenantInfo
	Rows         []clientRow
	Message      string
	Error        string
	Form         clientForm
	// CanAssign は、行のチェックボックスとまとめての付け替えを出すか（システム管理者。画面仕様 §7.1）。Back は付け替えの後の戻り先。
	CanAssign bool
	Back      string
}

// loadClients は一覧を集める。失敗したらステータスと文を返す（d は登録のフォームを出せる状態にしておく）。
func (h *Handler) loadClients(r *http.Request, d *clientsData) (int, string) {
	t, err := h.tenants(r)
	if err != nil {
		h.log.Error("tenants", "error", err)
		return http.StatusServiceUnavailable, "データベースに接続できません。しばらくしてから開き直してください。"
	}
	d.Tenants = t
	f, ok := readTenantFilter(r, t)
	d.Filter = f
	if !ok {
		return http.StatusBadRequest, "テナントの指定が正しくありません。"
	}
	clients, err := h.pv.ListRADIUSClients(r.Context())
	if err != nil {
		h.log.Warn("list radius clients", "error", err)
		fail := h.apiError(r, err, "")
		return fail.Status, fail.Message
	}
	ctx, cancel := dbContext(r)
	assigned, err := h.db.ClientTenants(ctx)
	cancel()
	if err != nil {
		h.log.Error("client tenants", "error", err)
		return http.StatusServiceUnavailable, "データベースに接続できません。しばらくしてから開き直してください。"
	}
	a := userFrom(r.Context()).Actor()
	include := func(tenant int64) bool {
		if !authz.Allowed(a, authz.ClientView, tenant) {
			return false
		}
		switch {
		case f.All:
			return true
		case f.None:
			return tenant == authz.NoTenant
		}
		return tenant == f.Tenant
	}
	seen := map[int64]bool{}
	for _, c := range clients {
		seen[c.ID] = true
		if tenant := assigned[c.ID]; include(tenant) {
			d.Rows = append(d.Rows, clientRow{ID: c.ID, Client: c, Tenant: tenant})
		}
	}
	// 所属の記録があるのに provisioner にないもの（本BFF の外で削除されたもの）も、範囲の中なら出す（画面仕様 §1.3）。
	for id, tenant := range assigned {
		if !seen[id] && include(tenant) {
			d.Rows = append(d.Rows, clientRow{ID: id, Missing: true, Tenant: tenant})
		}
	}
	// IP アドレスの順（provisioner の順）。下流にないものは末尾に ID の順で出す。
	slices.SortStableFunc(d.Rows, func(x, y clientRow) int {
		if x.Missing != y.Missing {
			if x.Missing {
				return 1
			}
			return -1
		}
		if x.Missing {
			return cmp.Compare(x.ID, y.ID)
		}
		return 0
	})
	return http.StatusOK, ""
}

// newClientsData は一覧の画面の値の土台を作る。
func newClientsData(r *http.Request) clientsData {
	return clientsData{Staff: isStaff(r), CanCreate: can(r, authz.ClientCreate), ChooseTenant: isStaff(r),
		Form: clientForm{IdemKey: newIdemKey(), Errors: fieldErrors{}}, CanAssign: can(r, authz.AssignmentUpdate), Back: "/clients"}
}

func (h *Handler) clients(w http.ResponseWriter, r *http.Request) {
	d := newClientsData(r)
	d.Back = r.URL.RequestURI()
	if d.CanAssign {
		d.Message, d.Error = bulkResult(r)
	}
	status, msg := h.loadClients(r, &d)
	d.Error = cmp.Or(msg, d.Error)
	if id, err := strconv.ParseInt(r.URL.Query().Get("deleted"), 10, 64); err == nil && id > 0 {
		d.Message = "RADIUSクライアント #" + strconv.FormatInt(id, 10) + " を削除しました。"
		if r.URL.Query().Get("unassign") == "failed" {
			d.Message += "（所属の記録を消せませんでした。下流にない所属の記録として残ります。）"
		}
	}
	h.render(w, r, status, "clients", "RADIUSクライアント", d)
}

// renderClients は一覧の画面（htmx では一覧と登録フォームの部分だけ）を返す。
func (h *Handler) renderClients(w http.ResponseWriter, r *http.Request, status int, d clientsData) {
	if d.Form.IdemKey == "" {
		d.Form.IdemKey = newIdemKey()
	}
	h.renderPart(w, r, status, "clients", "clients-section", "RADIUSクライアント", d)
}

// clientCreate は RADIUSクライアントを登録し、所属を記録する。テナント管理者は自分のテナントに、運用担当・システム管理者は
// 選んだテナント（またはテナントなし）に属させる（設計概要 §7）。ID は provisioner（本PoC）が採番する。
func (h *Handler) clientCreate(w http.ResponseWriter, r *http.Request) {
	if !can(r, authz.ClientCreate) {
		h.forbidden(w, r)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	f := readClientForm(r, true)
	a := userFrom(r.Context()).Actor()
	fail := func(status int, msg string) {
		d := newClientsData(r)
		_, loadMsg := h.loadClients(r, &d)
		f.Secret = ""
		d.Form, d.Error = f, cmp.Or(msg, loadMsg)
		h.renderClients(w, r, status, d)
	}
	tenant := a.TenantID
	if !a.Role.TenantScoped() {
		t, err := h.tenants(r)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		id, chosen, exists := readTenantChoice(f.Tenant, t)
		f.Errors.check(chosen, "tenant", "所属のテナントを選んでください。")
		f.Errors.check(!chosen || exists, "tenant", "選んだテナントはありません。画面を開き直してください。")
		tenant = id
	}
	if len(f.Errors) > 0 {
		fail(http.StatusBadRequest, "入力を確かめてください。")
		return
	}
	if !authz.Allowed(a, authz.ClientCreate, tenant) {
		h.forbidden(w, r)
		return
	}
	c, err := h.pv.CreateRADIUSClient(r.Context(), provisioner.RADIUSClientCreate{
		IP: f.IP, Secret: f.Secret, Name: f.Name, Vendor: f.Vendor,
	}, f.IdemKey)
	if err != nil {
		apiFail := h.apiError(r, err, "")
		h.log.Warn("create radius client", "error", err)
		f.IdemKey = keepIdemKey(err, f.IdemKey)
		fail(apiFail.Status, apiFail.Message)
		return
	}
	h.record(r, auditClientCreate, clientTarget(c.ID), tenant, map[string]any{"ip": c.IP, "name": c.Name, "vendor": c.Vendor})
	msg := "RADIUSクライアント " + c.Name + "（#" + strconv.FormatInt(c.ID, 10) + "、" + c.IP + "）を登録しました。"
	// 所属を記録する（provisioner の操作の後に書くので、失敗してもテナントなしとして残るだけ。§17.4 の 3）。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), dbTimeout)
	err = h.db.AssignClient(ctx, c.ID, tenant, a.Email)
	cancel()
	d := newClientsData(r)
	status, loadMsg := h.loadClients(r, &d)
	d.Message, d.Error = msg, loadMsg
	if err != nil {
		h.log.Error("assign client after create", "id", c.ID, "tenant", tenant, "error", err)
		d.Error = "RADIUSクライアントを登録しましたが、所属の記録に失敗しました（テナントなしになっています）。システム管理者に所属の付け替えを依頼してください。"
		if a.Role.TenantScoped() {
			// テナントなしになったものは、テナント管理者の一覧には出ない。
			d.Error += "（このクライアントは一覧に出ません。）"
		}
	}
	h.renderClients(w, r, status, d)
}

// ---- 詳細と変更 ----

type clientData struct {
	Client     provisioner.RADIUSClient
	Tenant     int64
	TenantName string
	// Staff はテナントを出すか（C・D）。CanEdit は変更・削除・共有シークレットの表示ができるか（B・C・D）。
	Staff, CanEdit bool
	// UpdateKey と DeleteKey は、変更と削除のフォームの Idempotency-Key。
	UpdateKey, DeleteKey string
	// CanAssign は所属の変更を出すか（システム管理者）。Tenants はその選択肢。
	CanAssign bool
	Tenants   tenantInfo
	Message   string
	Error     string
	// Form は変更の入力（誤りのときに戻す。共有シークレットは戻さない）。
	Form clientForm
}

// clientMissingMessage は、provisioner に RADIUSクライアントがないときの文。
func clientMissingMessage(id int64, tenant int64) string {
	what := "RADIUSクライアント #" + strconv.FormatInt(id, 10)
	if tenant != authz.NoTenant {
		return what + " は provisioner（本PoC）にありません。本BFF の外で削除された可能性があります。所属の記録はシステム管理者が消します。"
	}
	return notFoundMessage(what)
}

func (h *Handler) loadClient(r *http.Request, id, tenant int64) (clientData, *apiFailure) {
	a := userFrom(r.Context()).Actor()
	d := clientData{Tenant: tenant, Staff: isStaff(r), CanEdit: authz.Allowed(a, authz.ClientUpdate, tenant),
		UpdateKey: newIdemKey(), DeleteKey: newIdemKey(), Form: clientForm{Errors: fieldErrors{}},
		CanAssign: authz.Allowed(a, authz.AssignmentUpdate, tenant)}
	if d.Staff {
		t, err := h.tenants(r)
		if err != nil {
			h.log.Error("tenants", "error", err)
		}
		d.TenantName, d.Tenants = t.Name(tenant), t
	}
	c, err := h.pv.GetRADIUSClient(r.Context(), id)
	if err != nil {
		f := h.apiError(r, err, clientMissingMessage(id, tenant))
		if f.Status != http.StatusNotFound {
			h.log.Warn("get radius client", "error", err)
		}
		return d, &f
	}
	d.Client = c
	d.Form.IP, d.Form.Name, d.Form.Vendor = c.IP, c.Name, c.Vendor
	return d, nil
}

func (h *Handler) client(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathClientID(w, r)
	if !ok {
		return
	}
	tenant, ok := h.clientScope(w, r, id, authz.ClientView)
	if !ok {
		return
	}
	d, fail := h.loadClient(r, id, tenant)
	if fail != nil {
		h.renderFailure(w, r, *fail, "/clients", "RADIUSクライアントの一覧へ")
		return
	}
	h.render(w, r, http.StatusOK, "client", "RADIUSクライアント "+d.Client.Name, d)
}

// renderClient は操作の結果を詳細の画面（htmx では詳細の部分だけ）で返す。
func (h *Handler) renderClient(w http.ResponseWriter, r *http.Request, id, tenant int64, status int, opFail *apiFailure, mutate func(*clientData)) {
	d, fail := h.loadClient(r, id, tenant)
	if fail != nil {
		if opFail != nil {
			fail = opFail
		}
		h.renderFailure(w, r, *fail, "/clients", "RADIUSクライアントの一覧へ")
		return
	}
	mutate(&d)
	h.renderPart(w, r, status, "client", "client-detail", "RADIUSクライアント "+d.Client.Name, d)
}

// clientUpdate は IP アドレス・名前・ベンダー・共有シークレットを変更する（B・C・D）。
// 値が変わった項目と、入力のあった共有シークレットだけを送る。IP を変えても ID は変わらない。
func (h *Handler) clientUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathClientID(w, r)
	if !ok {
		return
	}
	tenant, ok := h.clientScope(w, r, id, authz.ClientUpdate)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	f := readClientForm(r, false)
	if len(f.Errors) > 0 {
		h.renderClient(w, r, id, tenant, http.StatusBadRequest, nil, func(d *clientData) {
			f.Secret = ""
			d.Form, d.Error, d.UpdateKey = f, "入力を確かめてください。", cmp.Or(f.IdemKey, d.UpdateKey)
		})
		return
	}
	cur, err := h.pv.GetRADIUSClient(r.Context(), id)
	if err != nil {
		h.renderFailure(w, r, h.apiError(r, err, clientMissingMessage(id, tenant)), "/clients", "RADIUSクライアントの一覧へ")
		return
	}
	var u provisioner.RADIUSClientUpdate
	var changed, fields []string
	detail := map[string]any{}
	if f.IP != cur.IP {
		u.IP, changed, fields = new(f.IP), append(changed, "IP アドレス"), append(fields, "ip")
		detail["ip"] = cur.IP + " -> " + f.IP
	}
	if f.Name != cur.Name {
		u.Name, changed, fields = new(f.Name), append(changed, "名前"), append(fields, "name")
	}
	if f.Vendor != cur.Vendor {
		u.Vendor, changed, fields = new(f.Vendor), append(changed, "ベンダー"), append(fields, "vendor")
	}
	if f.Secret != "" {
		u.Secret, changed, fields = new(f.Secret), append(changed, "共有シークレット"), append(fields, "secret")
	}
	if len(changed) == 0 {
		h.renderClient(w, r, id, tenant, http.StatusOK, nil, func(d *clientData) { d.Message = "変更はありません。" })
		return
	}
	if _, err := h.pv.UpdateRADIUSClient(r.Context(), id, u, f.IdemKey); err != nil {
		fail := h.apiError(r, err, clientMissingMessage(id, tenant))
		h.log.Warn("update radius client", "error", err)
		if fail.Status == http.StatusNotFound {
			h.renderFailure(w, r, fail, "/clients", "RADIUSクライアントの一覧へ")
			return
		}
		h.renderClient(w, r, id, tenant, fail.Status, &fail, func(d *clientData) {
			f.Secret = ""
			d.Form, d.Error, d.UpdateKey = f, fail.Message, keepIdemKey(err, f.IdemKey)
		})
		return
	}
	detail["fields"] = fields
	h.record(r, auditClientUpdate, clientTarget(id), tenant, detail)
	h.renderClient(w, r, id, tenant, http.StatusOK, nil, func(d *clientData) {
		d.Message = strings.Join(changed, "、") + " を変更しました。すぐに反映されます。"
	})
}

type secretData struct {
	ID     int64
	Secret string
	Shown  bool
	Error  string
}

// clientSecret は共有シークレットを表示する（B・C・D）。取得は provisioner・本PoCと本BFF の監査ログに残る。
func (h *Handler) clientSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathClientID(w, r)
	if !ok {
		return
	}
	tenant, ok := h.clientScope(w, r, id, authz.ClientSecretView)
	if !ok {
		return
	}
	secret, err := h.pv.GetRADIUSClientSecret(r.Context(), id)
	if err != nil {
		f := h.apiError(r, err, clientMissingMessage(id, tenant))
		h.log.Warn("get radius client secret", "error", err)
		h.renderBlock(w, r, f.Status, "client", "client-secret", secretData{ID: id, Error: f.Message})
		return
	}
	h.record(r, auditClientSecretRead, clientTarget(id), tenant, nil)
	h.renderBlock(w, r, http.StatusOK, "client", "client-secret", secretData{ID: id, Secret: secret, Shown: true})
}

// clientDelete は RADIUSクライアントを削除し、所属の記録も消す（B・C・D）。
func (h *Handler) clientDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathClientID(w, r)
	if !ok {
		return
	}
	tenant, ok := h.clientScope(w, r, id, authz.ClientUpdate)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	idem := formIdemKey(r, "idem")
	if err := h.pv.DeleteRADIUSClient(r.Context(), id, idem); err != nil {
		fail := h.apiError(r, err, clientMissingMessage(id, tenant))
		h.log.Warn("delete radius client", "error", err)
		if fail.Status == http.StatusNotFound {
			h.renderFailure(w, r, fail, "/clients", "RADIUSクライアントの一覧へ")
			return
		}
		h.renderClient(w, r, id, tenant, fail.Status, &fail, func(d *clientData) {
			d.Error, d.DeleteKey = fail.Message, keepIdemKey(err, idem)
		})
		return
	}
	h.record(r, auditClientDelete, clientTarget(id), tenant, nil)
	to := "/clients?deleted=" + strconv.FormatInt(id, 10)
	if tenant != authz.NoTenant {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), dbTimeout)
		err := h.db.AssignClient(ctx, id, authz.NoTenant, userFrom(r.Context()).Email)
		cancel()
		if err != nil {
			h.log.Error("unassign client after delete", "id", id, "error", err)
			to += "&unassign=failed"
		}
	}
	seeOther(w, r, to)
}
