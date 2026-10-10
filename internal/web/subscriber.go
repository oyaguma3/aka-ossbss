package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

// 加入者の画面（画面仕様 §2）。加入者は provisioner の統合リソース（IMSI＋鍵の置き場所＋認可ポリシー＋停止の状態）として扱い、
// 所属（テナント）は本BFF の DB の記録で決める（設計概要 §7）。

const (
	// subscribersPerPage は provisioner の一覧をそのまま出すとき（C・D の「すべて」）の 1 ページの件数。
	subscribersPerPage = 50
	// tenantSubscribersPerPage は所属の記録から出すとき（A・B、C・D のテナントの絞り込み）の 1 ページの件数（設計概要 §15 の 7）。
	tenantSubscribersPerPage = 20
	// unassignedScanPage と unassignedScanMax は「テナントなし」の絞り込みで provisioner の一覧を読む 1 回の件数と、
	// 1 回の表示で調べる件数の上限（§17.4 の 8）。
	unassignedScanPage = 500
	unassignedScanMax  = 5000
)

// can は、利用者の役割に op があるか（A・B は自分のテナントのものとして判定する。登録の画面を出すかなどに使う）。
func can(r *http.Request, op authz.Op) bool {
	a := userFrom(r.Context()).Actor()
	return authz.Allowed(a, op, a.TenantID)
}

// ---- 一覧 ----

// subscriberRow は一覧の 1 行。
type subscriberRow struct {
	IMSI string
	// Sub は provisioner の加入者（Missing・Failed のときは nil）。
	Sub *provisioner.Subscriber
	// Missing は、所属の記録があるのに provisioner にないこと（下流にない）。Failed は取得に失敗したこと。
	Missing, Failed bool
	Tenant          int64
}

type subscriberListData struct {
	// Staff は、鍵の置き場所・SQN・AMF・食い違い・テナントの列を出すか（C・D）。
	Staff     bool
	CanCreate bool
	Prefix    string
	Filter    tenantFilter
	Tenants   tenantInfo
	Rows      []subscriberRow
	Cursor    string
	Next      string
	// Total は所属の記録の件数（所属の記録から出すときだけ。HasTotal で示す）。
	Total    int64
	HasTotal bool
	// Scanned は「テナントなし」で調べた件数、ScanStopped は上限で止めたこと、ScanLast は調べた最後の IMSI。
	Scanned     int
	ScanStopped bool
	ScanLast    string
	Message     string
	Error       string
}

func (h *Handler) subscribers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := subscriberListData{Staff: isStaff(r), CanCreate: can(r, authz.SubscriberCreate),
		Prefix: strings.TrimSpace(q.Get("prefix")), Cursor: q.Get("cursor")}
	if v := q.Get("deleted"); imsiPattern.MatchString(v) {
		d.Message = "加入者 " + v + " を削除しました（認可ポリシーも削除しました）。"
		if q.Get("unassign") == "failed" {
			d.Message += "（所属の記録を消せませんでした。下流にない所属の記録として残ります。）"
		}
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Tenants = t
	f, ok := readTenantFilter(r, t)
	d.Filter = f
	switch {
	case !ok:
		d.Error = "テナントの指定が正しくありません。"
	case d.Prefix != "" && !prefixDigits.MatchString(d.Prefix):
		d.Error = "IMSI の前方一致は 15 桁までの数字で指定してください。"
	case d.Cursor != "" && !imsiPattern.MatchString(d.Cursor):
		d.Error = "ページの位置の指定が正しくありません。"
	}
	if d.Error != "" {
		h.render(w, r, http.StatusBadRequest, "subscribers", "加入者", d)
		return
	}

	status := http.StatusOK
	switch {
	case f.All:
		status = h.listAllSubscribers(r, &d)
	case f.None:
		status = h.listUnassignedSubscribers(r, &d)
	default:
		status = h.listTenantSubscribers(r, &d, f.Tenant)
	}
	h.render(w, r, status, "subscribers", "加入者", d)
}

// listAllSubscribers は provisioner の一覧をそのまま出し、所属のテナントを添える（C・D の「すべて」）。
func (h *Handler) listAllSubscribers(r *http.Request, d *subscriberListData) int {
	l, err := h.pv.ListSubscribers(r.Context(), provisioner.ListParams{Prefix: d.Prefix, Cursor: d.Cursor, Limit: subscribersPerPage})
	if err != nil {
		return h.listFailed(r, d, "list subscribers", err)
	}
	imsis := make([]string, len(l.Items))
	for i, s := range l.Items {
		imsis[i] = s.IMSI
	}
	ctx, cancel := dbContext(r)
	assigned, err := h.db.SubscriberTenants(ctx, imsis)
	cancel()
	if err != nil {
		h.log.Error("subscriber tenants", "error", err)
		d.Error = "データベースに接続できません（所属のテナントを出せません）。"
		return http.StatusServiceUnavailable
	}
	for i := range l.Items {
		d.Rows = append(d.Rows, subscriberRow{IMSI: l.Items[i].IMSI, Sub: &l.Items[i], Tenant: assigned[l.Items[i].IMSI]})
	}
	d.Next = l.NextCursor
	return http.StatusOK
}

// listUnassignedSubscribers は、provisioner の一覧を読んで所属の記録のないものだけを出す（C・D の「テナントなし」）。
// 50 件たまるか、1 回の表示で 5,000 件を調べたところで止める（§17.4 の 8）。provisioner の続きの位置は IMSI なので、
// 調べ終えた IMSI から再開できる。
func (h *Handler) listUnassignedSubscribers(r *http.Request, d *subscriberListData) int {
	cursor := d.Cursor
	for d.Scanned < unassignedScanMax {
		l, err := h.pv.ListSubscribers(r.Context(), provisioner.ListParams{Prefix: d.Prefix, Cursor: cursor, Limit: unassignedScanPage})
		if err != nil {
			return h.listFailed(r, d, "list subscribers", err)
		}
		imsis := make([]string, len(l.Items))
		for i, s := range l.Items {
			imsis[i] = s.IMSI
		}
		ctx, cancel := dbContext(r)
		assigned, err := h.db.SubscriberTenants(ctx, imsis)
		cancel()
		if err != nil {
			h.log.Error("subscriber tenants", "error", err)
			d.Error = "データベースに接続できません（所属のテナントを確かめられません）。"
			return http.StatusServiceUnavailable
		}
		for i := range l.Items {
			s := &l.Items[i]
			d.Scanned++
			cursor = s.IMSI
			if _, ok := assigned[s.IMSI]; ok {
				continue
			}
			d.Rows = append(d.Rows, subscriberRow{IMSI: s.IMSI, Sub: s})
			if len(d.Rows) == subscribersPerPage {
				// このページの残りか、次のページがあれば続きがある。
				if i < len(l.Items)-1 || l.NextCursor != "" {
					d.Next = cursor
				}
				return http.StatusOK
			}
		}
		if l.NextCursor == "" {
			return http.StatusOK
		}
		cursor = l.NextCursor
	}
	d.ScanStopped, d.ScanLast, d.Next = true, cursor, cursor
	return http.StatusOK
}

// listTenantSubscribers は、テナントの所属の記録を IMSI の順に読み、各 IMSI を provisioner から並行して取得する
// （A・B と、C・D のテナントの絞り込み。設計概要 §7.1）。
func (h *Handler) listTenantSubscribers(r *http.Request, d *subscriberListData, tenant int64) int {
	ctx, cancel := dbContext(r)
	imsis, err := h.db.TenantSubscribers(ctx, tenant, d.Prefix, d.Cursor, tenantSubscribersPerPage+1)
	if err == nil {
		d.Total, err = h.db.CountTenantSubscribers(ctx, tenant, d.Prefix)
	}
	cancel()
	if err != nil {
		h.log.Error("tenant subscribers", "error", err)
		d.Error = "データベースに接続できません。しばらくしてから開き直してください。"
		return http.StatusServiceUnavailable
	}
	d.HasTotal = true
	if len(imsis) > tenantSubscribersPerPage {
		imsis = imsis[:tenantSubscribersPerPage]
		d.Next = imsis[len(imsis)-1]
	}
	d.Rows = make([]subscriberRow, len(imsis))
	errs := make([]error, len(imsis))
	forEachConcurrently(len(imsis), fetchConcurrency, func(i int) {
		row := subscriberRow{IMSI: imsis[i], Tenant: tenant}
		s, err := h.pv.GetSubscriber(r.Context(), imsis[i])
		switch {
		case err == nil:
			row.Sub = &s
		case isNotFound(err):
			row.Missing = true
		default:
			row.Failed, errs[i] = true, err
		}
		d.Rows[i] = row
	})
	for _, err := range errs {
		if err != nil {
			return h.listFailed(r, d, "get subscriber", err)
		}
	}
	return http.StatusOK
}

// listFailed は一覧の取得の失敗を d に書き、ステータスを返す。
func (h *Handler) listFailed(r *http.Request, d *subscriberListData, what string, err error) int {
	h.log.Warn(what, "error", err)
	f := h.apiError(r, err, "")
	d.Error = f.Message
	return f.Status
}

// ---- 登録 ----

type subscriberForm struct {
	// Tenant は所属のテナントの選択（"none" はテナントなし、空は未選択）。
	Tenant                  string
	IMSI, Ki, OPc, SQN, AMF string
	Default                 string
	Tenants                 tenantInfo
	PLMNMap                 []provisioner.PLMNEntry
	PLMNKnown               bool
	IdemKey                 string
	Errors                  fieldErrors
	Error, ErrorOperation   string
}

// withPLMNMap は、フォームに provisioner の PLMN マップ（鍵の置き場所の決め方の説明）を入れる。取得できなければ説明を省く。
func (h *Handler) withPLMNMap(r *http.Request, f *subscriberForm) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if st, err := h.pv.Status(ctx); err == nil && st.IsProvisioner() {
		f.PLMNMap, f.PLMNKnown = st.PLMNMap, true
	}
}

func (h *Handler) subscriberNew(w http.ResponseWriter, r *http.Request) {
	if !can(r, authz.SubscriberCreate) {
		h.forbidden(w, r)
		return
	}
	h.renderSubscriberForm(w, r, http.StatusOK, subscriberForm{SQN: "000000000000", AMF: "8000", Default: provisioner.PolicyDeny})
}

// readSubscriberForm は登録のフォームを読む（確かめない）。
func readSubscriberForm(r *http.Request) subscriberForm {
	pf := r.PostForm
	f := subscriberForm{
		Tenant: pf.Get("tenant"), IMSI: strings.TrimSpace(pf.Get("imsi")), Ki: normalizeHex(pf.Get("ki")), OPc: normalizeHex(pf.Get("opc")),
		SQN: normalizeHex(pf.Get("sqn")), AMF: normalizeHex(pf.Get("amf")), Default: pf.Get("default"),
		IdemKey: formIdemKey(r, "idem"),
	}
	if f.Default != provisioner.PolicyAllow {
		f.Default = provisioner.PolicyDeny
	}
	return f
}

// renderSubscriberForm は登録のフォームを返す（htmx ではフォームの部分だけ）。
func (h *Handler) renderSubscriberForm(w http.ResponseWriter, r *http.Request, status int, f subscriberForm) {
	if f.Tenants.names == nil {
		t, err := h.tenants(r)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		f.Tenants = t
	}
	h.withPLMNMap(r, &f)
	if f.IdemKey == "" {
		f.IdemKey = newIdemKey()
	}
	if f.Errors == nil {
		f.Errors = fieldErrors{}
	}
	h.renderPart(w, r, status, "subscriber_new", "subscriber-form", "加入者の登録", f)
}

// subscriberNewForm は、既定の動作を切り替えたときにフォームを描き直す（allow のときだけ確認ダイアログを付ける）。
// 入力はそのまま返し、provisioner には何も送らない。
func (h *Handler) subscriberNewForm(w http.ResponseWriter, r *http.Request) {
	if !can(r, authz.SubscriberCreate) {
		h.forbidden(w, r)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	h.renderSubscriberForm(w, r, http.StatusOK, readSubscriberForm(r))
}

// subscriberCreate は加入者を登録し（置き場所に鍵を作り、認可ポリシーを作る）、所属を記録する。運用担当・システム管理者だけ。
func (h *Handler) subscriberCreate(w http.ResponseWriter, r *http.Request) {
	if !can(r, authz.SubscriberCreate) {
		h.forbidden(w, r)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	f := readSubscriberForm(r)
	f.Tenants, f.Errors = t, fieldErrors{}
	tenant, chosen, exists := readTenantChoice(f.Tenant, t)
	f.Errors.check(chosen, "tenant", "所属のテナントを選んでください。")
	f.Errors.check(!chosen || exists, "tenant", "選んだテナントはありません。画面を開き直してください。")
	f.Errors.check(imsiPattern.MatchString(f.IMSI), "imsi", "IMSI は 15 桁の数字で入力してください。")
	f.Errors.check(hex128.MatchString(f.Ki), "ki", "Ki は 16 進 32 桁で入力してください。")
	f.Errors.check(hex128.MatchString(f.OPc), "opc", "OPc は 16 進 32 桁で入力してください。")
	f.Errors.check(sqnPattern.MatchString(f.SQN), "sqn", "SQN は 16 進 12 桁で入力してください。")
	f.Errors.check(amfPattern.MatchString(f.AMF), "amf", "AMF は 16 進 4 桁で入力してください。")
	if len(f.Errors) > 0 {
		f.Error = "入力を確かめてください。"
		h.renderSubscriberForm(w, r, http.StatusBadRequest, f)
		return
	}
	// 権限は選んだテナントで確かめる（運用担当・システム管理者はどのテナントでもよい）。
	if !authz.Allowed(userFrom(r.Context()).Actor(), authz.SubscriberCreate, tenant) {
		h.forbidden(w, r)
		return
	}
	sub, err := h.pv.CreateSubscriber(r.Context(), provisioner.SubscriberCreate{
		IMSI: f.IMSI, Ki: f.Ki, OPc: f.OPc, SQN: f.SQN, AMF: f.AMF,
		Policy: provisioner.PolicyPut{Default: f.Default, Rules: []provisioner.PolicyRule{}},
	}, f.IdemKey)
	if err != nil {
		fail := h.apiError(r, err, "")
		h.log.Warn("create subscriber", "error", err)
		f.Ki, f.OPc = "", "" // 秘密の値は画面に戻さない
		f.Error, f.ErrorOperation, f.IdemKey = fail.Message, fail.OperationID, keepIdemKey(err, f.IdemKey)
		h.renderSubscriberForm(w, r, fail.Status, f)
		return
	}
	h.record(r, auditSubscriberCreate, f.IMSI, tenant, map[string]any{
		"keyStore": string(sub.KeyStore), "amf": f.AMF, "sqn": f.SQN, "policyDefault": f.Default})
	to := "/subscribers/" + f.IMSI + "?created=1"
	// 所属を記録する（テナントなしなら、本BFF の外で削除されて残っていた記録を消す）。provisioner の操作の後に書くので、
	// 失敗してもテナントなしとして残るだけで、他のテナントに見えることはない（§17.4 の 3）。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), dbTimeout)
	err = h.db.AssignSubscriber(ctx, f.IMSI, tenant, userFrom(r.Context()).Email)
	cancel()
	if err != nil {
		h.log.Error("assign subscriber after create", "imsi", f.IMSI, "tenant", tenant, "error", err)
		to += "&assign=failed"
	}
	seeOther(w, r, to)
}

// ---- 詳細と変更 ----

type subscriberData struct {
	Sub    provisioner.Subscriber
	Tenant int64
	// TenantName は所属のテナントの名前（C・D に出す）。
	TenantName string
	// Staff は、鍵の置き場所・SQN・AMF・食い違い・テナントを出すか（C・D）。
	Staff bool
	// CanEditPolicy は認可ポリシーの編集（B・C・D）、CanKeys は Ki / OPc の表示と変更（C・D）、
	// CanAuth は AMF / SQN などの変更（D）、CanDelete は削除（C・D）、CanSuspend は停止・再開ができるか。
	CanEditPolicy, CanKeys, CanAuth, CanDelete, CanSuspend bool
	// AuthKey・DeleteKey・StatusKey は、変更・削除・停止と再開のフォームの Idempotency-Key。
	AuthKey, DeleteKey, StatusKey string
	Message                       string
	Error                         string
	ErrorOperation                string
	Errors                        fieldErrors
}

// missingMessage は、provisioner に加入者がないときの文。所属の記録があれば、本BFF の外で削除されたものと分かる。
func missingMessage(imsi string, tenant int64) string {
	if tenant != authz.NoTenant {
		return "加入者 " + imsi + " は provisioner（本PoC・aka-only-server）にありません。本BFF の外で削除された可能性があります。所属の記録はシステム管理者が消します。"
	}
	return notFoundMessage("加入者 " + imsi)
}

// loadSubscriber は詳細の画面に出す値を集める。取得できなければ失敗を返す。
func (h *Handler) loadSubscriber(r *http.Request, imsi string, tenant int64) (subscriberData, *apiFailure) {
	a := userFrom(r.Context()).Actor()
	d := subscriberData{Tenant: tenant, Staff: isStaff(r),
		CanEditPolicy: authz.Allowed(a, authz.SubscriberPolicyUpdate, tenant),
		CanKeys:       authz.Allowed(a, authz.SubscriberKeysUpdate, tenant),
		CanAuth:       authz.Allowed(a, authz.SubscriberAuthUpdate, tenant),
		CanDelete:     authz.Allowed(a, authz.SubscriberDelete, tenant),
		CanSuspend:    authz.Allowed(a, authz.SubscriberSuspend, tenant),
		AuthKey:       newIdemKey(), DeleteKey: newIdemKey(), StatusKey: newIdemKey(), Errors: fieldErrors{}}
	if d.Staff {
		t, err := h.tenants(r)
		if err != nil {
			h.log.Error("tenants", "error", err)
		}
		d.TenantName = t.Name(tenant)
	}
	sub, err := h.pv.GetSubscriber(r.Context(), imsi)
	if err != nil {
		f := h.apiError(r, err, missingMessage(imsi, tenant))
		if f.Status != http.StatusNotFound {
			h.log.Warn("get subscriber", "error", err)
		}
		return d, &f
	}
	d.Sub = sub
	return d, nil
}

func (h *Handler) subscriber(w http.ResponseWriter, r *http.Request) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return
	}
	tenant, ok := h.subscriberScope(w, r, imsi, authz.SubscriberView)
	if !ok {
		return
	}
	d, fail := h.loadSubscriber(r, imsi, tenant)
	if fail != nil {
		h.renderFailure(w, r, *fail, "/subscribers", "加入者の一覧へ")
		return
	}
	q := r.URL.Query()
	if q.Get("created") == "1" {
		d.Message = "加入者を登録しました（認可ポリシーも作りました）。"
		if q.Get("assign") == "failed" {
			d.Error = "加入者を登録しましたが、所属の記録に失敗しました（テナントなしになっています）。システム管理者に所属の付け替えを依頼してください。"
		}
	}
	h.render(w, r, http.StatusOK, "subscriber", "加入者 "+imsi, d)
}

// renderSubscriber は操作の結果を詳細の画面（htmx では詳細の部分だけ）で返す。操作の失敗（opFail）の後に加入者を
// 読み直せなかった場合は、読み直しの失敗ではなく操作の失敗をエラーの画面で返す。
func (h *Handler) renderSubscriber(w http.ResponseWriter, r *http.Request, imsi string, tenant int64, status int, opFail *apiFailure,
	mutate func(*subscriberData)) {
	d, fail := h.loadSubscriber(r, imsi, tenant)
	if fail != nil {
		if opFail != nil {
			fail = opFail
		}
		h.renderFailure(w, r, *fail, "/subscribers", "加入者の一覧へ")
		return
	}
	mutate(&d)
	h.renderPart(w, r, status, "subscriber", "subscriber-detail", "加入者 "+imsi, d)
}

type keysData struct {
	IMSI  string
	Keys  *provisioner.SubscriberKeys
	Error string
}

// subscriberKeys は Ki / OPc を表示する（運用担当・システム管理者）。取得は provisioner・下流と本BFF の監査ログに残る。
func (h *Handler) subscriberKeys(w http.ResponseWriter, r *http.Request) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return
	}
	tenant, ok := h.subscriberScope(w, r, imsi, authz.SubscriberKeysView)
	if !ok {
		return
	}
	keys, err := h.pv.GetSubscriberKeys(r.Context(), imsi)
	if err != nil {
		f := h.apiError(r, err, missingMessage(imsi, tenant))
		h.log.Warn("get subscriber keys", "error", err)
		h.renderBlock(w, r, f.Status, "subscriber", "subscriber-keys", keysData{IMSI: imsi, Error: f.Message})
		return
	}
	h.record(r, auditSubscriberKeysRead, imsi, tenant, nil)
	h.renderBlock(w, r, http.StatusOK, "subscriber", "subscriber-keys", keysData{IMSI: imsi, Keys: &keys})
}

// authOnlyFields は、AMF / SQN などの変更（システム管理者だけ）のフォームの項目。運用担当が送ってきたら断る。
var authOnlyFields = []string{"amf", "sqn", "sqn_type", "allow_plain"}

// subscriberAuth は鍵の属性を変更する。運用担当は Ki / OPc だけ、システム管理者は AMF / SQN（aka-only-server の加入者は
// SQN の増加タイプ・平文HTTP の許可も）も変えられる（設計概要 §6.2）。入力のあった Ki / OPc と、値が変わった項目だけを送る。
func (h *Handler) subscriberAuth(w http.ResponseWriter, r *http.Request) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return
	}
	tenant, ok := h.subscriberScope(w, r, imsi, authz.SubscriberKeysUpdate)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	pf := r.PostForm
	canAuth := authz.Allowed(userFrom(r.Context()).Actor(), authz.SubscriberAuthUpdate, tenant)
	if !canAuth {
		for _, name := range authOnlyFields {
			if pf.Has(name) {
				h.forbidden(w, r)
				return
			}
		}
	}
	cur, err := h.pv.GetSubscriber(r.Context(), imsi)
	if err != nil {
		h.renderFailure(w, r, h.apiError(r, err, missingMessage(imsi, tenant)), "/subscribers", "加入者の一覧へ")
		return
	}
	if cur.Key == nil {
		h.renderSubscriber(w, r, imsi, tenant, http.StatusConflict, nil, func(d *subscriberData) {
			d.Error = "この加入者には鍵がないため、変更できません。加入者を削除してから登録し直してください。"
		})
		return
	}
	idem := formIdemKey(r, "idem")
	ki, opc := normalizeHex(pf.Get("ki")), normalizeHex(pf.Get("opc"))
	sqn, amf := normalizeHex(pf.Get("sqn")), normalizeHex(pf.Get("amf"))
	aka := cur.KeyStore == provisioner.KeyStoreAKA
	sqnType, allowPlain := pf.Get("sqn_type"), pf.Get("allow_plain") == "1"
	errs := fieldErrors{}
	errs.check(ki == "" || hex128.MatchString(ki), "ki", "Ki は 16 進 32 桁で入力してください。")
	errs.check(opc == "" || hex128.MatchString(opc), "opc", "OPc は 16 進 32 桁で入力してください。")
	if canAuth {
		errs.check(sqnPattern.MatchString(sqn), "sqn", "SQN は 16 進 12 桁で入力してください。")
		errs.check(amfPattern.MatchString(amf), "amf", "AMF は 16 進 4 桁で入力してください。")
		if aka {
			errs.check(sqnType == "inc1" || sqnType == "inc32" || sqnType == "inc33", "sqnType", "SQN の増加タイプを選んでください。")
		}
	}
	if len(errs) > 0 {
		h.renderSubscriber(w, r, imsi, tenant, http.StatusBadRequest, nil, func(d *subscriberData) {
			d.Errors, d.Error, d.AuthKey = errs, "入力を確かめてください。", idem
		})
		return
	}

	var u provisioner.SubscriberUpdate
	var changed, fields []string
	if ki != "" {
		u.Ki, changed, fields = ki, append(changed, "Ki"), append(fields, "ki")
	}
	if opc != "" {
		u.OPc, changed, fields = opc, append(changed, "OPc"), append(fields, "opc")
	}
	if canAuth {
		// SQN は変えたときだけ送る（送らなければ、認証で進んだ SQN に触れない）。
		if sqn != cur.Key.SQN {
			u.SQN, changed, fields = sqn, append(changed, "SQN"), append(fields, "sqn")
		}
		if amf != cur.Key.AMF {
			u.AMF, changed, fields = amf, append(changed, "AMF"), append(fields, "amf")
		}
		if aka && sqnType != cur.Key.SQNType {
			u.SQNType, changed, fields = sqnType, append(changed, "SQN の増加タイプ"), append(fields, "sqnType")
		}
		if aka && (cur.Key.AllowPlain == nil || allowPlain != *cur.Key.AllowPlain) {
			u.AllowPlain, changed, fields = new(allowPlain), append(changed, "平文HTTP の許可"), append(fields, "allowPlain")
		}
	}
	if len(changed) == 0 {
		h.renderSubscriber(w, r, imsi, tenant, http.StatusOK, nil, func(d *subscriberData) { d.Message = "変更はありません。" })
		return
	}
	if _, err := h.pv.UpdateSubscriber(r.Context(), imsi, u, idem); err != nil {
		fail := h.apiError(r, err, missingMessage(imsi, tenant))
		h.log.Warn("update subscriber", "error", err)
		h.renderSubscriber(w, r, imsi, tenant, fail.Status, &fail, func(d *subscriberData) {
			d.Error, d.ErrorOperation, d.AuthKey = fail.Message, fail.OperationID, keepIdemKey(err, idem)
		})
		return
	}
	h.record(r, auditSubscriberUpdate, imsi, tenant, map[string]any{"keyStore": string(cur.KeyStore), "fields": fields})
	h.renderSubscriber(w, r, imsi, tenant, http.StatusOK, nil, func(d *subscriberData) {
		d.Message = strings.Join(changed, "、") + " を変更しました。"
	})
}

// subscriberDelete は加入者を削除し（認可ポリシーと、置き場所の鍵を消す）、所属の記録も消す。運用担当・システム管理者だけ。
func (h *Handler) subscriberDelete(w http.ResponseWriter, r *http.Request) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return
	}
	tenant, ok := h.subscriberScope(w, r, imsi, authz.SubscriberDelete)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	idem := formIdemKey(r, "idem")
	if err := h.pv.DeleteSubscriber(r.Context(), imsi, idem); err != nil {
		fail := h.apiError(r, err, missingMessage(imsi, tenant))
		h.log.Warn("delete subscriber", "error", err)
		if fail.Status == http.StatusNotFound {
			// 既にないので、詳細を出し直せない。
			h.renderFailure(w, r, fail, "/subscribers", "加入者の一覧へ")
			return
		}
		h.renderSubscriber(w, r, imsi, tenant, fail.Status, &fail, func(d *subscriberData) {
			d.Error, d.ErrorOperation, d.DeleteKey = fail.Message, fail.OperationID, keepIdemKey(err, idem)
		})
		return
	}
	h.record(r, auditSubscriberDelete, imsi, tenant, nil)
	to := "/subscribers?deleted=" + imsi
	if tenant != authz.NoTenant {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), dbTimeout)
		err := h.db.AssignSubscriber(ctx, imsi, authz.NoTenant, userFrom(r.Context()).Email)
		cancel()
		if err != nil {
			h.log.Error("unassign subscriber after delete", "imsi", imsi, "error", err)
			to += "&unassign=failed"
		}
	}
	seeOther(w, r, to)
}

// ---- 停止・再開 ----

// formPolicyStatus は停止・再開のフォームの status（変更後の状態）を返す。active / suspended でなければ false。
func formPolicyStatus(r *http.Request) (string, bool) {
	v := r.PostForm.Get("status")
	return v, v == provisioner.PolicyActive || v == provisioner.PolicySuspended
}

// policyStatusMessage は停止・再開が済んだときの文。
func policyStatusMessage(status string) string {
	if status == provisioner.PolicySuspended {
		return "停止しました。次の認証から拒否します（接続中のセッションは切れません）。"
	}
	return "再開しました。次の認証から認可ポリシーのルールに従います。"
}

// changePolicyStatus は加入者を停止・再開し（認可ポリシーの状態を変える）、本BFF の監査ログに残す。
// 加入者の詳細と認可ポリシーの画面で共通。
func (h *Handler) changePolicyStatus(r *http.Request, imsi string, tenant int64, status, idem string) (provisioner.Policy, *apiFailure) {
	p, err := h.pv.SetPolicyStatus(r.Context(), imsi, status, idem)
	if err != nil {
		f := h.apiError(r, err, notFoundMessage("IMSI "+imsi+" の認可ポリシー"))
		h.log.Warn("set policy status", "error", err)
		return provisioner.Policy{}, &f
	}
	action := auditPolicyResume
	if status == provisioner.PolicySuspended {
		action = auditPolicySuspend
	}
	h.record(r, action, imsi, tenant, nil)
	return p, nil
}

// subscriberStatus は加入者の詳細の画面から停止・再開を行い、詳細を返す。全員が使える（A・B は自分のテナントのものだけ）。
func (h *Handler) subscriberStatus(w http.ResponseWriter, r *http.Request) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return
	}
	tenant, ok := h.subscriberScope(w, r, imsi, authz.SubscriberSuspend)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	status, ok := formPolicyStatus(r)
	if !ok {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return
	}
	idem := formIdemKey(r, "status_idem")
	p, fail := h.changePolicyStatus(r, imsi, tenant, status, idem)
	if fail != nil {
		h.renderSubscriber(w, r, imsi, tenant, fail.Status, fail, func(d *subscriberData) {
			d.Error, d.ErrorOperation = fail.Message, fail.OperationID
			if fail.Unreached && idem != "" {
				d.StatusKey = idem
			}
		})
		return
	}
	h.renderSubscriber(w, r, imsi, tenant, http.StatusOK, nil, func(d *subscriberData) { d.Message = policyStatusMessage(p.Status) })
}
