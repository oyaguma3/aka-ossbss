package web

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

// 認可ポリシーの編集の画面（画面仕様 §3）。加入者の詳細から開き、認可ポリシーだけの一覧・作成・削除は持たない（§17.4 の 1）。
// 既存の BFF の policy.go を元にした。

// maxPolicyRules は編集画面で扱うルールの数の上限。
const maxPolicyRules = 100

// ruleForm は編集中のルール。入力をそのまま持つ（保存するまで確かめない）。
type ruleForm struct {
	NASID string
	// SSIDs は許可する SSID を 1 行に 1 つずつ書いたもの。
	SSIDs          string
	VLANID         string
	SessionTimeout string
	// Errors はこのルールの項目ごとの誤り（キーは nasId / allowedSsids / vlanId / sessionTimeout）。
	Errors fieldErrors
	// Index は 0 からの番号、No は画面に出す 1 からの番号。First / Last は先頭・末尾か（上へ・下へのボタンに使う）。
	Index, No   int
	First, Last bool
}

type policyData struct {
	IMSI string
	// Exists は認可ポリシーが保存済みか。
	Exists bool
	// NoKey は、加入者に鍵がないか（運用担当・システム管理者にだけ出す。ShowKey で示す）。
	NoKey, ShowKey bool
	Default        string
	Rules          []ruleForm
	// Status は保存済みの認可ポリシーの状態（active / suspended。停止・再開に対応していない provisioner では空）。
	Status string
	// Dirty は、保存していない変更があるか。
	Dirty bool
	// IdemKey は保存の、StatusKey は停止・再開の Idempotency-Key。
	IdemKey, StatusKey string
	Errors             fieldErrors
	Message            string
	Error              string
	ErrorOperation     string
}

// formFromPolicy は保存済みのポリシーを編集の形にする。
func formFromPolicy(p provisioner.PolicyPut) (string, []ruleForm) {
	rules := make([]ruleForm, len(p.Rules))
	for i, rule := range p.Rules {
		rules[i] = ruleForm{
			NASID: rule.NASID, SSIDs: strings.Join(rule.AllowedSSIDs, "\n"), VLANID: rule.VLANID,
			Errors: fieldErrors{},
		}
		if rule.SessionTimeout != 0 {
			rules[i].SessionTimeout = strconv.Itoa(rule.SessionTimeout)
		}
	}
	return p.Default, rules
}

// policyScope は URL の IMSI を読み、利用者が認可ポリシーを変更してよいかを確かめる（B・C・D。B は自分のテナントのものだけ）。
func (h *Handler) policyScope(w http.ResponseWriter, r *http.Request, op authz.Op) (string, int64, bool) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return "", 0, false
	}
	tenant, ok := h.subscriberScope(w, r, imsi, op)
	return imsi, tenant, ok
}

func (h *Handler) policy(w http.ResponseWriter, r *http.Request) {
	imsi, tenant, ok := h.policyScope(w, r, authz.SubscriberPolicyUpdate)
	if !ok {
		return
	}
	sub, err := h.pv.GetSubscriber(r.Context(), imsi)
	if err != nil {
		f := h.apiError(r, err, missingMessage(imsi, tenant))
		if f.Status != http.StatusNotFound {
			h.log.Warn("get subscriber", "error", err)
		}
		h.renderFailure(w, r, f, "/subscribers", "加入者の一覧へ")
		return
	}
	d := policyData{IMSI: imsi, Default: provisioner.PolicyDeny, ShowKey: isStaff(r), NoKey: sub.Key == nil,
		IdemKey: newIdemKey(), StatusKey: newIdemKey(), Errors: fieldErrors{}}
	if sub.Policy != nil {
		// 認可ポリシーがなければ、既定は Admin TUI と同じく deny でルールなし。保存すると作成する。
		d.Exists, d.Status = true, sub.Status
		d.Default, d.Rules = formFromPolicy(*sub.Policy)
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

// readPolicyForm は編集中のフォーム（ルールは同じ名前の項目の並び）を読む。
// exists・no_key・state（保存済みの状態）・dirty（保存していない変更があるか）は、画面に出し直すために持ち回る値。
func readPolicyForm(r *http.Request) (policyData, error) {
	pf := r.PostForm
	d := policyData{
		Default: pf.Get("default"), Exists: pf.Get("exists") == "1", NoKey: pf.Get("no_key") == "1", ShowKey: isStaff(r),
		Status: pf.Get("state"), Dirty: pf.Get("dirty") == "1", IdemKey: formIdemKey(r, "idem"), StatusKey: formIdemKey(r, "status_idem"),
		Errors: fieldErrors{},
	}
	nas, ssids, vlans, timeouts := pf["nas_id"], pf["ssids"], pf["vlan_id"], pf["session_timeout"]
	if len(ssids) != len(nas) || len(vlans) != len(nas) || len(timeouts) != len(nas) || len(nas) > maxPolicyRules {
		return d, errors.New("rule fields mismatch")
	}
	for i := range nas {
		d.Rules = append(d.Rules, ruleForm{
			NASID: nas[i], SSIDs: ssids[i], VLANID: vlans[i], SessionTimeout: timeouts[i], Errors: fieldErrors{},
		})
	}
	return d, nil
}

// renderPolicy は編集の画面（htmx では編集の部分だけ）を返す。
func (h *Handler) renderPolicy(w http.ResponseWriter, r *http.Request, status int, d policyData) {
	numberRules(d.Rules)
	if d.IdemKey == "" {
		d.IdemKey = newIdemKey()
	}
	if d.StatusKey == "" {
		d.StatusKey = newIdemKey()
	}
	h.renderPart(w, r, status, "policy", "policy-detail", "認可ポリシー "+d.IMSI, d)
}

// readPolicyRequest は編集・保存・停止と再開の要求のフォームを読む。読めなければエラーの画面を返して false。
func (h *Handler) readPolicyRequest(w http.ResponseWriter, r *http.Request, imsi string) (policyData, bool) {
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return policyData{}, false
	}
	d, err := readPolicyForm(r)
	if err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return policyData{}, false
	}
	d.IMSI = imsi
	return d, true
}

// policyEdit はルールの追加・削除・並べ替えを行った編集画面を返す。保存はしない（provisioner には送らない）。
// op は add / remove:N / up:N / down:N（N は 0 からのルールの番号）。空なら入力をそのまま返す（既定の動作の切り替え）。
func (h *Handler) policyEdit(w http.ResponseWriter, r *http.Request) {
	imsi, _, ok := h.policyScope(w, r, authz.SubscriberPolicyUpdate)
	if !ok {
		return
	}
	d, ok := h.readPolicyRequest(w, r, imsi)
	if !ok {
		return
	}
	d.Dirty = true
	op, arg, _ := strings.Cut(r.PostForm.Get("op"), ":")
	i, convErr := strconv.Atoi(arg)
	inRange := convErr == nil && i >= 0 && i < len(d.Rules)
	switch {
	case op == "add":
		if len(d.Rules) >= maxPolicyRules {
			d.Error = fmt.Sprintf("ルールは %d 件までです。", maxPolicyRules)
			break
		}
		// 新しいルールは、任意の NAS に一致する形で末尾に足す（上から順に評価するので、必要なら上へ動かす）。
		d.Rules = append(d.Rules, ruleForm{NASID: "*", Errors: fieldErrors{}})
	case op == "remove" && inRange:
		d.Rules = slices.Delete(d.Rules, i, i+1)
	case op == "up" && inRange && i > 0:
		d.Rules[i-1], d.Rules[i] = d.Rules[i], d.Rules[i-1]
	case op == "down" && inRange && i < len(d.Rules)-1:
		d.Rules[i], d.Rules[i+1] = d.Rules[i+1], d.Rules[i]
	}
	h.renderPolicy(w, r, http.StatusOK, d)
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

// toPolicyPut は編集中のフォームを確かめ、provisioner に送る形にする。誤りは d に書き込み、false を返す。
func toPolicyPut(d *policyData) (provisioner.PolicyPut, bool) {
	ok := true
	if d.Default != provisioner.PolicyAllow && d.Default != provisioner.PolicyDeny {
		d.Errors["default"] = "既定の動作を選んでください。"
		ok = false
	}
	put := provisioner.PolicyPut{Default: d.Default, Rules: []provisioner.PolicyRule{}}
	for i := range d.Rules {
		f := &d.Rules[i]
		f.NASID, f.VLANID, f.SessionTimeout = strings.TrimSpace(f.NASID), strings.TrimSpace(f.VLANID), strings.TrimSpace(f.SessionTimeout)
		rule := provisioner.PolicyRule{NASID: f.NASID, AllowedSSIDs: splitSSIDs(f.SSIDs), VLANID: f.VLANID}
		f.Errors.check(nasIDPattern.MatchString(f.NASID), "nasId",
			"NAS-ID は空白を含まない英数字・記号の 253 文字までで入力してください（* はすべての NAS）。")
		f.Errors.check(len(rule.AllowedSSIDs) > 0, "allowedSsids", "許可する SSID を 1 つ以上入力してください（* はすべての SSID）。")
		for j, s := range rule.AllowedSSIDs {
			f.Errors.check(len(s) <= maxSSIDLen, "allowedSsids",
				fmt.Sprintf("%d 行目の SSID が長すぎます（%d バイトまで）。", j+1, maxSSIDLen))
		}
		if f.VLANID != "" {
			v, err := strconv.Atoi(f.VLANID)
			f.Errors.check(vlanIDPattern.MatchString(f.VLANID) && err == nil && v <= maxVLANID, "vlanId",
				fmt.Sprintf("VLAN ID は 0〜%d の数字で入力してください（空なら割り当てない）。", maxVLANID))
		}
		if f.SessionTimeout != "" {
			v, err := strconv.Atoi(f.SessionTimeout)
			f.Errors.check(err == nil && v >= 0 && v <= maxSessionTimeout, "sessionTimeout",
				fmt.Sprintf("Session-Timeout は 0〜%d の秒数で入力してください（空か 0 なら送らない）。", maxSessionTimeout))
			rule.SessionTimeout = v
		}
		if len(f.Errors) > 0 {
			ok = false
		}
		put.Rules = append(put.Rules, rule)
	}
	return put, ok
}

// applyInvalidParams は provisioner の invalidParams を、該当するルールの項目の誤りとして書き込む。
// すべて対応付けられたら true を返す。
func applyInvalidParams(d *policyData, params []provisioner.InvalidParam) bool {
	all := true
	for _, p := range params {
		if p.Param == "default" {
			d.Errors.check(false, "default", "既定の動作を選んでください。")
			continue
		}
		m := ruleParamPattern.FindStringSubmatch(p.Param)
		if m == nil {
			all = false
			continue
		}
		i, err := strconv.Atoi(m[1])
		if err != nil || i >= len(d.Rules) {
			all = false
			continue
		}
		msg := ruleFieldLabels[m[2]] + " が正しくありません。"
		if m[2] == "allowedSsids" && m[3] != "" {
			j, _ := strconv.Atoi(m[3])
			msg = fmt.Sprintf("%d 行目の SSID が正しくありません。", j+1)
		}
		d.Rules[i].Errors.check(false, m[2], msg)
	}
	return all
}

// policySave は編集中の内容で認可ポリシー全体を置き換える（なければ作る）。状態（停止中かどうか）は変わらない。
func (h *Handler) policySave(w http.ResponseWriter, r *http.Request) {
	imsi, tenant, ok := h.policyScope(w, r, authz.SubscriberPolicyUpdate)
	if !ok {
		return
	}
	d, ok := h.readPolicyRequest(w, r, imsi)
	if !ok {
		return
	}
	d.Dirty = true
	put, valid := toPolicyPut(&d)
	if !valid {
		d.Error = "入力を確かめてください。"
		h.renderPolicy(w, r, http.StatusBadRequest, d)
		return
	}
	p, created, err := h.pv.PutPolicy(r.Context(), imsi, put, d.IdemKey)
	if err != nil {
		f := h.apiError(r, err, "")
		h.log.Warn("put policy", "error", err)
		if apiErr, ok := errors.AsType[*provisioner.Error](err); ok && apiErr.Status == http.StatusBadRequest &&
			applyInvalidParams(&d, apiErr.Problem.InvalidParams) {
			f.Message = "入力を確かめてください。"
		}
		d.Error, d.ErrorOperation, d.IdemKey = f.Message, f.OperationID, keepIdemKey(err, d.IdemKey)
		h.renderPolicy(w, r, f.Status, d)
		return
	}
	h.record(r, auditPolicyPut, imsi, tenant, map[string]any{"created": created, "default": p.Default, "rules": len(p.Rules)})
	saved := policyData{IMSI: imsi, Exists: true, NoKey: d.NoKey, ShowKey: d.ShowKey, Status: p.Status, StatusKey: d.StatusKey, Errors: fieldErrors{}}
	saved.Default, saved.Rules = formFromPolicy(provisioner.PolicyPut{Default: p.Default, Rules: p.Rules})
	saved.Message = "保存しました。すぐに認可に反映されます。"
	if created {
		saved.Message = "作成しました。すぐに認可に反映されます。"
	}
	h.renderPolicy(w, r, http.StatusOK, saved)
}

// policyStatus は認可ポリシーの画面から停止・再開を行う。編集中のフォームも一緒に受け取り、保存していない
// ルールの変更は保ったまま（保存はしない）状態だけを変えて返す。
func (h *Handler) policyStatus(w http.ResponseWriter, r *http.Request) {
	// 停止・再開の権限で確かめる（認可ポリシーの画面を開けるのは B・C・D だが、停止・再開は全員の操作）。
	imsi, tenant, ok := h.policyScope(w, r, authz.SubscriberSuspend)
	if !ok {
		return
	}
	d, ok := h.readPolicyRequest(w, r, imsi)
	if !ok {
		return
	}
	status, ok := formPolicyStatus(r)
	if !ok {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return
	}
	p, fail := h.changePolicyStatus(r, imsi, tenant, status, d.StatusKey)
	if fail != nil {
		d.Error, d.ErrorOperation = fail.Message, fail.OperationID
		if !fail.Unreached {
			d.StatusKey = ""
		}
		h.renderPolicy(w, r, fail.Status, d)
		return
	}
	d.Exists, d.Status, d.StatusKey = true, p.Status, ""
	if !d.Dirty {
		// 編集中の変更がなければ、最新の内容を出す（他の操作で変わっていても分かるように）。
		d.Default, d.Rules = formFromPolicy(provisioner.PolicyPut{Default: p.Default, Rules: p.Rules})
	}
	d.Message = policyStatusMessage(p.Status)
	h.renderPolicy(w, r, http.StatusOK, d)
}
