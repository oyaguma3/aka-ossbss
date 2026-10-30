package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/ledger"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 認可ポリシーの編集（設計概要 §8.1。段階 3-1 の W8）。加入者の詳細から開く。ルールのテナントは台帳の所属のテナントに
// 固定し、画面では選ばせない（F4）。NAS-Identifier は自由に書け、`*` も使える（登録した AP から選ぶのは段階 4）。
// 保存は aka-radius の PATCH（rules。全体を置き換える）。使えるのはテナント管理者・運用担当・システム管理者。

// ruleForm は編集中のルール。入力をそのまま持つ（保存するまで確かめない）。
type ruleForm struct {
	NASID string
	// SSIDs は SSID を 1 行に 1 つずつ書いたもの（* はすべて）。
	SSIDs          string
	VLANID         string
	SessionTimeout string
	// TenantID は保存されているルールのテナント（所属のテナントと違えば、保存で直すことを示す）。
	TenantID int64
	// Errors はこのルールの項目ごとの誤り（キーは nasId / ssids / vlanId / sessionTimeout）。
	Errors fieldErrors
	// Index は 0 からの番号、No は画面に出す 1 からの番号。First / Last は先頭・末尾か（上へ・下へのボタンに使う）。
	Index, No   int
	First, Last bool
}

// ruleFromAPI は保存済みのルールを編集の形にする。
func ruleFromAPI(r provisioner.Rule) ruleForm {
	f := ruleForm{NASID: r.NASID, SSIDs: strings.Join(r.SSIDs, "\n"), TenantID: r.TenantID, Errors: fieldErrors{}}
	if r.VLANID != 0 {
		f.VLANID = strconv.Itoa(r.VLANID)
	}
	if r.SessionTimeout != 0 {
		f.SessionTimeout = strconv.Itoa(r.SessionTimeout)
	}
	return f
}

// splitSSIDs は 1 行に 1 つずつ書いた SSID を読む。前後の空白を除き、空行は無視する。
func splitSSIDs(v string) []string {
	var out []string
	for line := range strings.Lines(v) {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toRule は編集中のルールを確かめ、テナント tenant のルールにする。誤りは f.Errors に書き込み、false を返す。
func (f *ruleForm) toRule(tenant int64) (provisioner.Rule, bool) {
	if f.Errors == nil {
		f.Errors = fieldErrors{}
	}
	f.NASID, f.VLANID, f.SessionTimeout = strings.TrimSpace(f.NASID), strings.TrimSpace(f.VLANID), strings.TrimSpace(f.SessionTimeout)
	rule := provisioner.Rule{TenantID: tenant, NASID: f.NASID, SSIDs: splitSSIDs(f.SSIDs)}
	f.Errors.check(nasIDPattern.MatchString(f.NASID), "nasId", "NAS-Identifier を 253 文字までで入力してください（* はすべての AP）。")
	f.Errors.check(len(rule.SSIDs) > 0, "ssids", "SSID を 1 つ以上入力してください（* はすべての SSID）。")
	f.Errors.check(len(rule.SSIDs) <= maxSSIDs, "ssids", fmt.Sprintf("SSID は %d 個までです。", maxSSIDs))
	f.Errors.check(!slices.Contains(rule.SSIDs, "*") || len(rule.SSIDs) == 1, "ssids", "* はほかの SSID と一緒に書けません。")
	for j, s := range rule.SSIDs {
		f.Errors.check(len(s) <= maxSSIDLen, "ssids", fmt.Sprintf("%d 行目の SSID が長すぎます（%d バイトまで）。", j+1, maxSSIDLen))
	}
	if f.VLANID != "" {
		v, err := strconv.Atoi(f.VLANID)
		f.Errors.check(err == nil && v >= 1 && v <= maxVLANID, "vlanId", fmt.Sprintf("VLAN は 1〜%d の数字で入力してください（空なら付けない）。", maxVLANID))
		rule.VLANID = v
	}
	if f.SessionTimeout != "" {
		v, err := strconv.Atoi(f.SessionTimeout)
		f.Errors.check(err == nil && v >= 1 && v <= maxSessionTimeout, "sessionTimeout",
			fmt.Sprintf("Session-Timeout は 1〜%d の秒数で入力してください（空なら付けない）。", maxSessionTimeout))
		rule.SessionTimeout = v
	}
	return rule, len(f.Errors) == 0
}

type policyData struct {
	IMSI string
	// Tenant と TenantName は所属のテナント（ルールのテナントに入れる）。
	Tenant     int64
	TenantName string
	Rules      []ruleForm
	// Terminated は radius 側が解約で、変更できないこと。
	Terminated bool
	// Dirty は、保存していない変更があるか。TenantDiffers は、保存されているルールに所属と違うテナントがあるか。
	Dirty, TenantDiffers bool
	IdemKey              string
	Message, Error       string
}

// policyScope は URL の IMSI を読み、利用者が認可ポリシーを変更してよいかを確かめる（B・C・D。B は自分のテナントのものだけ）。
func (h *Handler) policyScope(w http.ResponseWriter, r *http.Request) (db.Entry, bool) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return db.Entry{}, false
	}
	e, ok := h.subscriberScope(w, r, imsi, authz.PolicyUpdate)
	if !ok {
		return db.Entry{}, false
	}
	if !e.RadiusPresent {
		h.renderErrorLink(w, r, http.StatusNotFound, "この加入者は radius 側に登録されていません（認可ポリシーは radius 側にあります）。",
			"/subscribers/"+imsi, "加入者の詳細へ")
		return db.Entry{}, false
	}
	return e, true
}

// newPolicyData は台帳の行から編集の画面の値を作る。
func (h *Handler) newPolicyData(r *http.Request, e db.Entry) policyData {
	d := policyData{IMSI: e.IMSI, Tenant: e.TenantID, TenantName: "#" + formatID(e.TenantID), Terminated: e.RadiusState == db.StateTerminated}
	if t, err := h.tenants(r); err == nil {
		d.TenantName = t.Name(e.TenantID)
	}
	return d
}

func (h *Handler) policy(w http.ResponseWriter, r *http.Request) {
	e, ok := h.policyScope(w, r)
	if !ok {
		return
	}
	rs, err := h.pv.RadiusSubscriber(r.Context(), e.IMSI)
	if err != nil {
		f := h.apiError(r, err, db.SideRadius)
		h.log.Warn("get radius subscriber", "error", err)
		h.renderFailure(w, r, f, "/subscribers/"+e.IMSI, "加入者の詳細へ")
		return
	}
	d := h.newPolicyData(r, e)
	d.Terminated = rs.State == db.StateTerminated
	for _, rule := range rs.Rules {
		f := ruleFromAPI(rule)
		d.TenantDiffers = d.TenantDiffers || f.TenantID != e.TenantID
		d.Rules = append(d.Rules, f)
	}
	h.renderPolicy(w, r, http.StatusOK, d)
}

// numberRules はルールに番号と位置を書き込む。
func numberRules(rules []ruleForm) {
	for i := range rules {
		rules[i].Index, rules[i].No = i, i+1
		rules[i].First, rules[i].Last = i == 0, i == len(rules)-1
	}
}

// renderPolicy は編集の画面（htmx では編集の部分だけ）を返す。
func (h *Handler) renderPolicy(w http.ResponseWriter, r *http.Request, status int, d policyData) {
	numberRules(d.Rules)
	for i := range d.Rules {
		if d.Rules[i].Errors == nil {
			d.Rules[i].Errors = fieldErrors{}
		}
	}
	if d.IdemKey == "" {
		d.IdemKey = newIdemKey()
	}
	h.renderPart(w, r, status, "policy", "policy-detail", "認可ポリシー "+d.IMSI, d)
}

// readPolicyRequest は編集・保存の要求のフォーム（ルールは同じ名前の項目の並び）を読む。読めなければエラーの画面を返して false。
func (h *Handler) readPolicyRequest(w http.ResponseWriter, r *http.Request, e db.Entry) (policyData, bool) {
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return policyData{}, false
	}
	pf := r.PostForm
	d := h.newPolicyData(r, e)
	d.Dirty, d.IdemKey = pf.Get("dirty") == "1", formIdemKey(r, "idem")
	nas, ssids, vlans, timeouts := pf["nas_id"], pf["ssids"], pf["vlan_id"], pf["session_timeout"]
	if len(ssids) != len(nas) || len(vlans) != len(nas) || len(timeouts) != len(nas) || len(nas) > maxRules+1 {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return policyData{}, false
	}
	for i := range nas {
		d.Rules = append(d.Rules, ruleForm{NASID: nas[i], SSIDs: ssids[i], VLANID: vlans[i], SessionTimeout: timeouts[i],
			TenantID: e.TenantID, Errors: fieldErrors{}})
	}
	return d, true
}

// policyEdit はルールの追加・削除・並べ替えを行った編集の画面を返す。保存はしない（aka-provisioner には送らない）。
// op は add / remove:N / up:N / down:N（N は 0 からのルールの番号）。
func (h *Handler) policyEdit(w http.ResponseWriter, r *http.Request) {
	e, ok := h.policyScope(w, r)
	if !ok {
		return
	}
	d, ok := h.readPolicyRequest(w, r, e)
	if !ok {
		return
	}
	d.Dirty = true
	before := len(d.Rules)
	d.Rules = applyRuleOp(d.Rules, r.PostForm.Get("op"), e.TenantID)
	if len(d.Rules) > maxRules {
		d.Rules = d.Rules[:before]
		d.Error = fmt.Sprintf("ルールは %d 件までです。", maxRules)
	}
	h.renderPolicy(w, r, http.StatusOK, d)
}

// applyRuleOp はルールの編集の操作を行う。op は add / remove:N / up:N / down:N（N は 0 からのルールの番号）。
// 新しいルール（テナントは tenant）は、すべての AP・SSID に一致する形で末尾に足す（上から順に評価するので、必要なら上へ動かす）。
// add の後の件数は呼び出し側で確かめる。
func applyRuleOp(rules []ruleForm, op string, tenant int64) []ruleForm {
	name, arg, _ := strings.Cut(op, ":")
	i, convErr := strconv.Atoi(arg)
	inRange := convErr == nil && i >= 0 && i < len(rules)
	switch {
	case name == "add":
		rules = append(rules, ruleForm{NASID: "*", SSIDs: "*", TenantID: tenant, Errors: fieldErrors{}})
	case name == "remove" && inRange:
		rules = slices.Delete(rules, i, i+1)
	case name == "up" && inRange && i > 0:
		rules[i-1], rules[i] = rules[i], rules[i-1]
	case name == "down" && inRange && i < len(rules)-1:
		rules[i], rules[i+1] = rules[i+1], rules[i]
	}
	return rules
}

// applyInvalidParams は aka-radius の invalidParams を、該当するルールの項目の誤りとして書き込む。
// すべて対応付けられたら true を返す。
func applyInvalidParams(d *policyData, params []provisioner.InvalidParam) bool {
	all := true
	for _, p := range params {
		m := ruleParamPattern.FindStringSubmatch(p.Param)
		if m == nil {
			all = false
			continue
		}
		i, err := strconv.Atoi(m[1])
		if err != nil || i >= len(d.Rules) || m[2] == "tenantId" {
			all = false
			continue
		}
		d.Rules[i].Errors.check(false, m[2], ruleFieldLabels[m[2]]+" が正しくありません。")
	}
	return all
}

// policySave は編集中のルールで認可ポリシー全体を置き換える（ルールのテナントは所属のテナント）。
func (h *Handler) policySave(w http.ResponseWriter, r *http.Request) {
	e, ok := h.policyScope(w, r)
	if !ok {
		return
	}
	d, ok := h.readPolicyRequest(w, r, e)
	if !ok {
		return
	}
	d.Dirty = true
	rules := make([]provisioner.Rule, 0, len(d.Rules))
	valid := len(d.Rules) >= 1 && len(d.Rules) <= maxRules
	for i := range d.Rules {
		rule, ok := d.Rules[i].toRule(e.TenantID)
		valid = valid && ok
		rules = append(rules, rule)
	}
	if !valid {
		d.Error = "入力を確かめてください。"
		if len(d.Rules) == 0 || len(d.Rules) > maxRules {
			d.Error = fmt.Sprintf("ルールは 1〜%d 件にしてください（どのルールにも一致しない接続は拒否します）。", maxRules)
		}
		h.renderPolicy(w, r, http.StatusBadRequest, d)
		return
	}
	o, err := h.runLedger(r, e.IMSI, ledger.Step{
		Check: func(cur db.Entry, exists bool) string {
			switch {
			case !exists || !cur.RadiusPresent:
				return "この加入者は radius 側に登録されていません。"
			case cur.TenantID != e.TenantID:
				return "所属のテナントが他の操作で変わりました。画面を開き直してください。"
			}
			return ""
		},
		Call: func(ctx context.Context, _ db.Entry) error {
			_, err := h.pv.UpdateRadiusSubscriber(ctx, e.IMSI, provisioner.RadiusSubscriberUpdate{Rules: rules}, d.IdemKey)
			return err
		},
	})
	switch {
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	case o.Refused != "":
		d.Error = o.Refused
		h.renderPolicy(w, r, http.StatusConflict, d)
		return
	case o.Unknown:
		d.Error = "保存の結果が分かりませんでした（aka-provisioner か下流が応答しませんでした）。同じ内容でもう一度保存するか、開き直して確かめてください。"
		h.renderPolicy(w, r, http.StatusServiceUnavailable, d)
		return
	case o.Err != nil:
		f := h.apiError(r, o.Err, db.SideRadius)
		h.log.Warn("save rules", "error", o.Err)
		if apiErr, ok := errors.AsType[*provisioner.Error](o.Err); ok && apiErr.Status == http.StatusBadRequest &&
			applyInvalidParams(&d, apiErr.Problem.InvalidParams) {
			f.Message = "入力を確かめてください。"
		}
		d.Error, d.IdemKey = f.Message, keepIdemKey(o.Err, d.IdemKey)
		h.renderPolicy(w, r, f.Status, d)
		return
	}
	h.record(r, auditPolicyUpdate, e.IMSI, e.TenantID, map[string]any{"rules": len(rules)})
	saved := h.newPolicyData(r, e)
	for _, rule := range rules {
		saved.Rules = append(saved.Rules, ruleFromAPI(rule))
	}
	saved.Message = "保存しました。次の認証から使います。"
	h.renderPolicy(w, r, http.StatusOK, saved)
}
