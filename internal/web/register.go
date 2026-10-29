package web

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 加入者の個別の登録（設計概要 §8。段階 3-1 の W4）。vecgen 側（システム管理者。鍵を入力）と radius 側（運用担当・
// システム管理者）の 2 つのフォーム。所属のテナントは最初に登録した側で選び、もう一方の側では台帳の値で固定する。
// 削除済みの IMSI を登録し直すときは、システム管理者の承認（確認と理由）を求め、監査ログに残す。
// Ki・OPc は aka-provisioner に送るだけで、DB・ログ・監査ログに残さず、画面にも戻さない。

// registerForm は登録のフォーム（両側で共通の項目と、側ごとの項目）。
type registerForm struct {
	Side db.Side
	IMSI string
	// Tenant はテナントの選択（台帳にない・削除済みの IMSI のとき）。FixedTenant は台帳の所属（もう一方の側を登録済みのとき）。
	Tenant      string
	FixedTenant int64
	Tenants     tenantInfo
	// Deleted は削除済みの IMSI（登録し直すには承認が要る）。Approve と ApproveReason は承認の入力。
	Deleted       bool
	Approve       bool
	ApproveReason string
	State         string
	// vecgen 側。
	AMF, SQN, SQNType string
	Clients           []int64
	AllClients        []provisioner.VecgenClient
	// radius 側（取得先と最初のルール）。
	Source  string
	Sources []provisioner.Source
	Rule    ruleForm
	// CanVecgen と CanRadius は、それぞれのフォームを出せるか（切り替えのタブ）。
	CanVecgen, CanRadius bool
	IdemKey              string
	Errors               fieldErrors
	Error                string
	// LoadError は選択肢（許可クライアント・取得先）を読めなかったときの文。
	LoadError string
}

// ShowTenantChoice は、テナントを選ばせるか（台帳にない・削除済み）。
func (f registerForm) ShowTenantChoice() bool { return f.FixedTenant == 0 }

func (h *Handler) subscriberNew(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := registerForm{Side: db.Side(q.Get("side")), IMSI: strings.TrimSpace(q.Get("imsi")), State: db.StateActive,
		AMF: "8000", SQN: "000000000000", SQNType: "inc32", Rule: ruleForm{NASID: "*", SSIDs: "*"}, Errors: fieldErrors{}}
	f.CanVecgen, f.CanRadius = can(r, authz.VecgenRegister), can(r, authz.RadiusRegister)
	if f.Side != db.SideVecgen && f.Side != db.SideRadius {
		f.Side = db.SideRadius
		if f.CanVecgen {
			f.Side = db.SideVecgen
		}
	}
	if !h.canRegister(r, f.Side) {
		h.forbidden(w, r)
		return
	}
	if imsiPattern.MatchString(f.IMSI) {
		ctx, cancel := dbContext(r)
		e, err := h.db.Subscriber(ctx, f.IMSI)
		cancel()
		switch {
		case errors.Is(err, db.ErrNotFound):
		case err != nil:
			h.dbUnavailable(w, r, err)
			return
		case e.Deleted():
			f.Deleted = true
		default:
			f.FixedTenant = e.TenantID
		}
	}
	h.renderRegister(w, r, http.StatusOK, f)
}

// canRegister は、利用者がその側を登録できるか。
func (h *Handler) canRegister(r *http.Request, side db.Side) bool {
	if side == db.SideVecgen {
		return can(r, authz.VecgenRegister)
	}
	return can(r, authz.RadiusRegister)
}

// renderRegister は登録のフォームを返す（htmx ではフォームの部分だけ）。選択肢は描くたびに読む。
func (h *Handler) renderRegister(w http.ResponseWriter, r *http.Request, status int, f registerForm) {
	if f.Tenants.names == nil {
		t, err := h.tenants(r)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		f.Tenants = t
	}
	f.CanVecgen, f.CanRadius = can(r, authz.VecgenRegister), can(r, authz.RadiusRegister)
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()
	var err error
	if f.Side == db.SideVecgen {
		f.AllClients, err = h.pv.VecgenClients(ctx)
	} else {
		f.Sources, err = h.pv.RadiusSources(ctx)
	}
	if err != nil {
		h.log.Warn("register form: load choices", "side", f.Side, "error", err)
		f.LoadError = "選択肢を aka-provisioner から読めませんでした: " + h.apiError(r, err, f.Side).Message
	}
	if f.IdemKey == "" {
		f.IdemKey = newIdemKey()
	}
	if f.Errors == nil {
		f.Errors = fieldErrors{}
	}
	h.renderPart(w, r, status, "subscriber_new", "register-form", "加入者の登録", f)
}

// readRegisterForm は登録のフォームの共通の項目を読む（確かめない）。
func readRegisterForm(r *http.Request, side db.Side) registerForm {
	pf := r.PostForm
	return registerForm{Side: side, IMSI: strings.TrimSpace(pf.Get("imsi")), Tenant: pf.Get("tenant"), State: pf.Get("state"),
		Approve: pf.Get("approve") == "1", ApproveReason: strings.TrimSpace(pf.Get("approve_reason")),
		IdemKey: formIdemKey(r, "idem"), Errors: fieldErrors{}}
}

// registerTarget は、台帳から登録の先（所属のテナント、削除済みか）を決めて確かめる。f.Errors・f.Error に誤りを書き、
// 登録できるならテナントを返す。fresh は、新しく登録を始める（台帳にない・削除済み）か。
func (h *Handler) registerTarget(w http.ResponseWriter, r *http.Request, f *registerForm) (tenant int64, fresh, ok bool) {
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return 0, false, false
	}
	f.Tenants = t
	if !imsiPattern.MatchString(f.IMSI) {
		f.Errors["imsi"] = "IMSI は 15 桁の数字で入力してください。"
		return 0, false, true
	}
	ctx, cancel := dbContext(r)
	e, err := h.db.Subscriber(ctx, f.IMSI)
	cancel()
	exists := err == nil
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		h.dbUnavailable(w, r, err)
		return 0, false, false
	}
	a := userFrom(r.Context()).Actor()
	switch {
	case exists && !e.Deleted():
		f.FixedTenant = e.TenantID
		if e.Present(f.Side) {
			f.Error = "この IMSI は" + sideLabel(f.Side) + "に登録済みです。"
		}
		return e.TenantID, false, true
	case exists:
		f.Deleted = true
		switch {
		case !authz.Allowed(a, authz.ReregisterApprove, authz.NoTenant):
			f.Error = "この IMSI は削除済みです。登録し直すにはシステム管理者の承認が要ります。システム管理者に依頼してください。"
		case !f.Approve || f.ApproveReason == "" || len([]rune(f.ApproveReason)) > maxReasonLen:
			f.Errors["approve"] = "削除済みの IMSI です。登録し直すなら、承認にチェックを入れ、理由（500 文字まで）を入力してください。"
		}
	}
	id, chosen, live := readTenantChoice(f.Tenant, t)
	f.Errors.check(chosen, "tenant", "所属のテナントを選んでください。")
	f.Errors.check(!chosen || live, "tenant", "選んだテナントはありません。画面を開き直してください。")
	return id, true, true
}

// registerStep は登録の操作（台帳の確かめ・下流の呼び出し・台帳の変更）を組み立てる。
func registerStep(f registerForm, tenant int64, fresh bool, call func(ctx context.Context) error) ledgerStep {
	return ledgerStep{
		Check: func(cur db.Entry, exists bool) string {
			// 確かめた後に台帳が変わっていれば、登録しない（所属のテナントを決め直す）。
			nowFresh := !exists || cur.Deleted()
			switch {
			case nowFresh != fresh, !fresh && cur.TenantID != tenant:
				return "台帳が他の操作で変わりました。画面を開き直してください。"
			case exists && cur.Present(f.Side):
				return "この IMSI は" + sideLabel(f.Side) + "に登録済みです。"
			}
			return ""
		},
		Call: func(ctx context.Context, _ db.Entry) error { return call(ctx) },
		Change: func(db.Entry, bool) db.Change {
			ch := db.Change{Kind: db.ChangeRegister, Side: f.Side, State: f.State, SourceID: f.Source}
			if fresh {
				ch.TenantID = tenant
			}
			return ch
		},
		Tenant: tenant,
	}
}

// finishRegister は登録の結果を返す（成功・結果が分からず台帳がある: 詳細へ。それ以外: フォームに誤り）。
func (h *Handler) finishRegister(w http.ResponseWriter, r *http.Request, f registerForm, tenant int64, o ledgerOutcome, err error,
	audit map[string]any) {
	action := auditVecgenCreate
	if f.Side == db.SideRadius {
		action = auditRadiusCreate
	}
	switch {
	case err != nil && !errors.Is(err, db.ErrTenantNotFound):
		f.Error = sideLabel(f.Side) + "への登録の後、台帳を書けませんでした（結果を確かめる印を付けました）。"
		h.renderRegister(w, r, http.StatusServiceUnavailable, f)
		return
	case err != nil:
		f.Errors["tenant"] = "選んだテナントは削除されました。"
		f.Error = "入力を確かめてください。"
		h.renderRegister(w, r, http.StatusConflict, f)
		return
	case o.Refused != "":
		f.Error = o.Refused
		h.renderRegister(w, r, http.StatusConflict, f)
		return
	case o.Err != nil && !(o.Unknown && o.Exists):
		f.IdemKey = keepIdemKey(o.Err, f.IdemKey)
		if o.Unknown {
			f.Error = unknownMessage(sideLabel(f.Side)+"への登録", o)
		} else {
			f.Error = h.apiError(r, o.Err, f.Side).Message
		}
		h.renderRegister(w, r, h.apiError(r, o.Err, f.Side).Status, f)
		return
	}
	audit["side"], audit["result"] = string(f.Side), resultWord(o, nil)
	h.record(r, action, f.IMSI, tenant, audit)
	if f.Deleted {
		h.record(r, auditReregister, f.IMSI, tenant, map[string]any{"side": string(f.Side), "reason": f.ApproveReason})
	}
	to := "/subscribers/" + f.IMSI + "?created=" + string(f.Side)
	if o.Unknown {
		to = "/subscribers/" + f.IMSI
	}
	seeOther(w, r, to)
}

// vecgenCreate は vecgen 側に登録する（システム管理者だけ。鍵を入力する）。
func (h *Handler) vecgenCreate(w http.ResponseWriter, r *http.Request) {
	if !can(r, authz.VecgenRegister) {
		h.forbidden(w, r)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	f := readRegisterForm(r, db.SideVecgen)
	pf := r.PostForm
	ki, opc := normalizeHex(pf.Get("ki")), normalizeHex(pf.Get("opc"))
	f.AMF, f.SQN, f.SQNType = normalizeHex(pf.Get("amf")), normalizeHex(pf.Get("sqn")), pf.Get("sqn_type")
	for _, v := range pf["clients"] {
		id, err := parseID(v)
		f.Errors.check(err == nil, "clients", "許可クライアントの指定が正しくありません。")
		f.Clients = append(f.Clients, id)
	}
	slices.Sort(f.Clients)
	f.Clients = slices.Compact(f.Clients)
	tenant, fresh, ok := h.registerTarget(w, r, &f)
	if !ok {
		return
	}
	f.Errors.check(hex128.MatchString(ki), "ki", "Ki は 16 進 32 桁で入力してください。")
	f.Errors.check(hex128.MatchString(opc), "opc", "OPc は 16 進 32 桁で入力してください。")
	f.Errors.check(amfPattern.MatchString(f.AMF), "amf", "AMF は 16 進 4 桁で入力してください。")
	f.Errors.check(sqnPattern.MatchString(f.SQN), "sqn", "SQN は 16 進 12 桁で入力してください。")
	f.Errors.check(slices.Contains(sqnTypes, f.SQNType), "sqnType", "SQN の方式を選んでください。")
	f.Errors.check(f.State == db.StateActive || f.State == db.StateInactive, "state", "登録のときの状態を選んでください。")
	if f.Error != "" || len(f.Errors) > 0 {
		h.renderRegister(w, r, registerFailStatus(&f), f)
		return
	}
	clients := f.Clients
	if clients == nil {
		clients = []int64{}
	}
	o, err := h.runLedger(r, f.IMSI, registerStep(f, tenant, fresh, func(ctx context.Context) error {
		_, err := h.pv.CreateVecgenSubscriber(ctx, provisioner.VecgenSubscriberCreate{IMSI: f.IMSI, Ki: ki, OPc: opc, State: f.State,
			SQN: f.SQN, AMF: f.AMF, SQNType: f.SQNType, AllowedClientIDs: clients}, f.IdemKey)
		return err
	}))
	h.finishRegister(w, r, f, tenant, o, err, map[string]any{"state": f.State, "amf": f.AMF, "sqn": f.SQN, "sqnType": f.SQNType,
		"allowedClientIds": clients})
}

// radiusCreate は radius 側に登録する（運用担当・システム管理者）。取得先と最初のルールを入れる（ルールのテナントは所属のテナント）。
func (h *Handler) radiusCreate(w http.ResponseWriter, r *http.Request) {
	if !can(r, authz.RadiusRegister) {
		h.forbidden(w, r)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	f := readRegisterForm(r, db.SideRadius)
	pf := r.PostForm
	f.Source = pf.Get("source")
	f.Rule = ruleForm{NASID: pf.Get("nas_id"), SSIDs: pf.Get("ssids"), VLANID: pf.Get("vlan_id"), SessionTimeout: pf.Get("session_timeout"),
		Errors: fieldErrors{}}
	tenant, fresh, ok := h.registerTarget(w, r, &f)
	if !ok {
		return
	}
	f.Errors.check(sourceIDPattern.MatchString(f.Source), "source", "取得先を選んでください。")
	f.Errors.check(f.State == db.StateActive || f.State == db.StateInactive, "state", "登録のときの状態を選んでください。")
	rule, ruleOK := f.Rule.toRule(tenant)
	if !ruleOK {
		f.Errors["rule"] = "ルールの入力を確かめてください。"
	}
	if f.Error != "" || len(f.Errors) > 0 || !ruleOK {
		h.renderRegister(w, r, registerFailStatus(&f), f)
		return
	}
	rules := []provisioner.Rule{rule}
	o, err := h.runLedger(r, f.IMSI, registerStep(f, tenant, fresh, func(ctx context.Context) error {
		_, err := h.pv.CreateRadiusSubscriber(ctx, provisioner.RadiusSubscriberCreate{IMSI: f.IMSI, State: f.State, SourceID: f.Source,
			Rules: rules}, f.IdemKey)
		return err
	}))
	h.finishRegister(w, r, f, tenant, o, err, map[string]any{"state": f.State, "sourceId": f.Source, "rules": len(rules)})
}

// registerFailStatus は、登録の前の確かめで断ったときの状態コードを返す（入力の誤りは 400、登録済み・承認がないなど台帳による断りは 409）。
func registerFailStatus(f *registerForm) int {
	if len(f.Errors) > 0 {
		f.Error = cmp.Or(f.Error, "入力を確かめてください。")
		return http.StatusBadRequest
	}
	return http.StatusConflict
}
