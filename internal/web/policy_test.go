package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// policyForm は認可ポリシーの編集のフォーム（ルールは nas_id 等の並び）。
func policyForm(def string, rules ...[4]string) url.Values {
	v := url.Values{"default": {def}, "exists": {"1"}, "state": {"active"}}
	for _, r := range rules {
		v.Add("nas_id", r[0])
		v.Add("ssids", r[1])
		v.Add("vlan_id", r[2])
		v.Add("session_timeout", r[3])
	}
	return v
}

func TestPolicyPage(t *testing.T) {
	e := newTestEnv()
	// テナント管理者には鍵の有無を出さない。
	s := e.pv.subs[imsiOwn]
	s.Key = nil
	e.pv.subs[imsiOwn] = s
	w := get(e.handler(t, "b@example.com"), "/subscribers/"+imsiOwn+"/policy")
	if w.Code != 200 {
		t.Fatalf("policy = %d %s", w.Code, w.Body)
	}
	mustContain(t, w.Body.String(), `value="ap-01"`, "corp", "停止する", `hx-include="#policy-form"`, `name="idem"`, `name="status_idem"`)
	mustNotContain(t, w.Body.String(), "鍵（Ki / OPc）が登録されていません", "まだありません")
	mustContain(t, get(e.handler(t, "c@example.com"), "/subscribers/"+imsiOwn+"/policy").Body.String(), "鍵（Ki / OPc）が登録されていません")
	// 認可ポリシーのない加入者は「保存」で作成する。
	s = e.pv.subs[imsiNone]
	s.Policy, s.Status = nil, ""
	e.pv.subs[imsiNone] = s
	body := get(e.handler(t, "c@example.com"), "/subscribers/"+imsiNone+"/policy").Body.String()
	mustContain(t, body, "この加入者の認可ポリシーはまだありません。「保存」で作成します。")
	mustNotContain(t, body, "停止する")
	// provisioner にない加入者は 404（認可ポリシーだけを作らない）。
	if w := get(e.handler(t, "d@example.com"), "/subscribers/001019999999999/policy"); w.Code != 404 {
		t.Errorf("missing = %d", w.Code)
	}
}

func TestPolicyEdit(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "b@example.com")
	path := "/subscribers/" + imsiOwn + "/policy/edit"
	form := policyForm("deny", [4]string{"ap-01", "corp", "10", ""}, [4]string{"ap-02", "guest", "", "3600"})
	form.Set("op", "add")
	body := post(h, path, form).Body.String()
	mustContain(t, body, "ルール 3", `value="*"`, "保存していない変更があります")
	form.Set("op", "up:1")
	body = post(h, path, form).Body.String()
	if strings.Index(body, `value="ap-02"`) > strings.Index(body, `value="ap-01"`) {
		t.Error("rule was not moved up")
	}
	form.Set("op", "remove:0")
	body = post(h, path, form).Body.String()
	mustNotContain(t, body, `value="ap-01"`)
	// allow に切り替えると、保存の前に確認ダイアログを出す（ダイアログはフォームの外）。
	form = policyForm("allow")
	body = post(h, path, form).Body.String()
	mustContain(t, body, `commandfor="dialog-save"`, `form="policy-form"`)
	formStart := strings.Index(body, `<form id="policy-form"`)
	if strings.Index(body, `<dialog id="dialog-save"`) < formStart+strings.Index(body[formStart:], "</form>") {
		t.Error("dialog is inside the form")
	}
	// ルールの数がそろっていないフォーム。
	bad := url.Values{"default": {"deny"}, "nas_id": {"a", "b"}, "ssids": {"x"}}
	if w := post(h, path, bad); w.Code != 400 {
		t.Errorf("mismatch = %d", w.Code)
	}
	if len(e.pv.ops) != 0 {
		t.Errorf("edit called provisioner: %v", e.pv.ops)
	}
}

func TestPolicySave(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "b@example.com")
	path := "/subscribers/" + imsiOwn + "/policy"
	// 入力の誤りは、該当するルールの項目に出す。
	form := policyForm("deny", [4]string{"ap 01", "", "5000", "-1"})
	w := post(h, path, form)
	if w.Code != 400 || len(e.pv.ops) != 0 {
		t.Fatalf("invalid = %d", w.Code)
	}
	mustContain(t, w.Body.String(), "NAS-ID は空白を含まない", "許可する SSID を 1 つ以上", "VLAN ID は 0〜4094", "Session-Timeout は 0〜86400")
	// 保存。
	form = policyForm("deny", [4]string{"ap-01", " corp \n\nguest", "20", "600"})
	form.Set("idem", "EEEEEEEEEEEEEEEEEEEEEEEEEE")
	w = post(h, path, form)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "保存しました。すぐに認可に反映されます。") {
		t.Fatalf("save = %d %s", w.Code, w.Body)
	}
	if got := e.pv.lastPut; len(got.Rules) != 1 || strings.Join(got.Rules[0].AllowedSSIDs, ",") != "corp,guest" ||
		got.Rules[0].VLANID != "20" || got.Rules[0].SessionTimeout != 600 {
		t.Errorf("put = %+v", got)
	}
	if e.pv.lastOp() != "PutPolicy b@example.com EEEEEEEEEEEEEEEEEEEEEEEEEE" {
		t.Errorf("op = %s", e.pv.lastOp())
	}
	if a := e.db.lastAudit(); a.Action != "policy.put" || a.TenantID != 10 || a.Detail["created"] != false || a.Detail["rules"] != 1 {
		t.Errorf("audit = %+v", a)
	}
	// 保存の後は新しいキーにする。
	mustNotContain(t, w.Body.String(), "EEEEEEEEEEEEEEEEEEEEEEEEEE", "保存していない変更があります")
	// provisioner の invalidParams を、該当するルールの項目に対応付ける。
	e.pv.errs["PutPolicy"] = &provisioner.Error{Status: 400, Problem: provisioner.Problem{Cause: provisioner.CauseMandatoryIEIncorrect,
		InvalidParams: []provisioner.InvalidParam{{Param: "rules[0].allowedSsids[1]"}}}}
	w = post(h, path, form)
	if w.Code != 400 {
		t.Errorf("invalid params = %d", w.Code)
	}
	mustContain(t, w.Body.String(), "入力を確かめてください。", "2 行目の SSID が正しくありません。")
}

// 認可ポリシーの画面の停止・再開は、保存していないルールの変更を残す（保存はしない）。
func TestPolicyStatusKeepsDraft(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "c@example.com")
	form := policyForm("deny", [4]string{"draft-ap", "draft", "", ""})
	form.Set("dirty", "1")
	form.Set("status", "suspended")
	w := post(h, "/subscribers/"+imsiOwn+"/policy/status", form)
	if w.Code != 200 {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	mustContain(t, w.Body.String(), `value="draft-ap"`, "停止しました。", "再開する", "保存していないルールの変更はそのまま残ります")
	if e.pv.subs[imsiOwn].Policy.Rules[0].NASID != "ap-01" {
		t.Error("draft was saved")
	}
	// 編集中の変更がなければ、最新の内容を出す。
	form = policyForm("deny")
	form.Set("status", "active")
	mustContain(t, post(h, "/subscribers/"+imsiOwn+"/policy/status", form).Body.String(), `value="ap-01"`, "再開しました。")
}
