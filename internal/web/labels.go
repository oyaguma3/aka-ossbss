package web

import (
	"cmp"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 画面に出す名前と、値の表示の形。

// issueLabels は、2 つのノードの状態の食い違い（provisioner の加入者の issues）の名前。
var issueLabels = map[provisioner.Issue]string{
	provisioner.IssueKeyMissing:            "鍵がありません",
	provisioner.IssuePolicyMissing:         "認可ポリシーがありません",
	provisioner.IssueKeyInOtherStore:       "置き場所でない方にも鍵があります",
	provisioner.IssueAVClientNotAllowed:    "vector-gateway の AVクライアントを許可していません",
	provisioner.IssueOtherStoreUnreachable: "置き場所でない方を確かめられませんでした",
}

// issueHelps は食い違いの説明と対処。
var issueHelps = map[provisioner.Issue]string{
	provisioner.IssueKeyMissing: "置き場所に加入者（Ki / OPc）がないため、認証できません。" +
		"認可ポリシーだけが残っている場合は、加入者を削除してから登録し直してください。",
	provisioner.IssuePolicyMissing: "認可ポリシーがないため、本PoCは認証を拒否します。認可ポリシーを作成してください。",
	provisioner.IssueKeyInOtherStore: "PLMN マップで決まる置き場所でない方にも、同じ IMSI の加入者があります。" +
		"本PoCと provisioner の PLMN マップが一致しているか、Admin TUI などで登録していないかを確かめ、要らない方を削除してください。",
	provisioner.IssueAVClientNotAllowed: "aka-vecgen の加入者が、本PoCの vector-gateway の AVクライアントを許可していないため、認証できません。" +
		"aka-vecgen の管理 GUI などで、許可するクライアントに加えてください。",
	provisioner.IssueOtherStoreUnreachable: "provisioner が置き場所でない方の下流に接続できず、そちらに同じ IMSI の加入者がないかを確かめられませんでした。" +
		"ダッシュボードで接続の状態を確かめてください。",
}

func issueLabel(i provisioner.Issue) string { return cmp.Or(issueLabels[i], string(i)) }
func issueHelp(i provisioner.Issue) string  { return issueHelps[i] }

// keyStoreLabel は鍵の置き場所の名前。
func keyStoreLabel(ks provisioner.KeyStore) string {
	switch ks {
	case provisioner.KeyStorePoC:
		return "本PoC"
	case provisioner.KeyStoreAKA:
		return "aka-vecgen"
	}
	return string(ks)
}

// policyStatusLabel は加入者の状態（認可ポリシーの状態）の名前を返す。状態のない（停止・再開に対応していない
// provisioner の）場合は空文字列。Valkey を直接書き換えた不正な値は、そのまま見せる（本PoCはこの加入者の認証を拒否する）。
func policyStatusLabel(status string) string {
	switch status {
	case "":
		return ""
	case provisioner.PolicyActive:
		return "利用中"
	case provisioner.PolicySuspended:
		return "停止中"
	}
	return "不明な状態（" + status + "）"
}

// formatOctets は通信量（octets）を、単位をつけた読みやすい形にする（例: 512 B、1.5 KiB、20.0 MiB）。
func formatOctets(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	v, i := float64(n)/unit, 0
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + "KMGTP"[i:i+1] + "iB"
}

// datetime は日時を画面の形（ローカル時刻）にする。ゼロ値は「-」。
func datetime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// opStatusLabels は操作の記録の状態の名前。
var opStatusLabels = map[string]string{
	provisioner.OpRunning:    "実行中",
	provisioner.OpCompleted:  "完了",
	provisioner.OpRolledBack: "元に戻した",
	provisioner.OpRetrying:   "やり直し中",
	provisioner.OpFailed:     "失敗（手での対応が必要）",
	provisioner.OpDismissed:  "閉じた",
}

func opStatusLabel(s string) string { return cmp.Or(opStatusLabels[s], s) }

// opKindLabel は操作の記録の種類の名前。
func opKindLabel(k string) string {
	return cmp.Or(map[string]string{
		"subscriber.create": "加入者の登録",
		"subscriber.update": "加入者の変更",
		"subscriber.delete": "加入者の削除",
	}[k], k)
}

// stepLabel は操作の記録の手順の名前。
func stepLabel(name string) string {
	return cmp.Or(map[string]string{
		"subscriber.create":     "加入者（鍵）の作成",
		"subscriber.update":     "加入者（鍵）の変更",
		"subscriber.delete":     "加入者（鍵）の削除",
		"subscriber.compensate": "作った加入者（鍵）の削除（元に戻す）",
		"policy.put":            "認可ポリシーの保存",
		"policy.delete":         "認可ポリシーの削除",
		"policy.restore":        "認可ポリシーを変更前に戻す",
	}[name], name)
}

// stepStateLabel は操作の記録の手順の状態の名前。
func stepStateLabel(state string) string {
	return cmp.Or(map[string]string{
		"pending":     "未実施",
		"done":        "済み",
		"failed":      "失敗",
		"compensated": "元に戻した",
		"skipped":     "不要",
	}[state], state)
}

// auditActions は本BFF の監査ログの操作と名前（画面の操作の種類の絞り込みの順。画面仕様 §1.7）。
var auditActions = []struct{ Action, Label string }{
	{auditSubscriberCreate, "加入者の登録"},
	{auditSubscriberUpdate, "加入者の変更"},
	{auditSubscriberDelete, "加入者の削除"},
	{auditSubscriberKeysRead, "Ki / OPc の表示"},
	{auditPolicyPut, "認可ポリシーの保存"},
	{auditPolicySuspend, "加入者の停止"},
	{auditPolicyResume, "加入者の再開"},
	{auditClientCreate, "RADIUSクライアントの登録"},
	{auditClientUpdate, "RADIUSクライアントの変更"},
	{auditClientDelete, "RADIUSクライアントの削除"},
	{auditClientSecretRead, "共有シークレットの表示"},
	{auditAssignmentUpdate, "所属の付け替え"},
	{auditAssignmentDelete, "所属の記録の削除"},
	{auditTenantCreate, "テナントの作成"},
	{auditTenantUpdate, "テナントの名前の変更"},
	{auditTenantDelete, "テナントの削除"},
	{auditAccountCreate, "アカウントの作成"},
	{auditAccountUpdate, "アカウントの変更"},
	{auditAccountDisable, "アカウントの無効化"},
	{auditAccountEnable, "アカウントの有効化"},
	{auditAccountDelete, "アカウントの削除"},
	{auditOperationRetry, "操作のやり直し"},
	{auditOperationDismiss, "操作を閉じる"},
}

// otherActionLabels は、provisioner と下流の監査ログにだけ出る操作の名前。
var otherActionLabels = map[string]string{
	"policy.create":        "認可ポリシーの作成",
	"policy.update":        "認可ポリシーの変更",
	"policy.delete":        "認可ポリシーの削除",
	"policy.status.update": "加入者の状態の変更",
	"operation.resume":     "自動のやり直し",
}

// actionLabel は監査ログの操作の名前を返す。
func actionLabel(action string) string {
	for _, a := range auditActions {
		if a.Action == action {
			return a.Label
		}
	}
	return cmp.Or(otherActionLabels[action], action)
}

// knownAction は、本BFF の監査ログの操作の名前か。
func knownAction(action string) bool {
	return slices.ContainsFunc(auditActions, func(a struct{ Action, Label string }) bool { return a.Action == action })
}

// detailText は監査ログの内容（JSON のオブジェクト）を、項目の名前の順に「名前: 値」で並べた 1 行にする。
// aka-vecgen の変更の { "from": …, "to": … } は「変更前 → 変更後」にする。
func detailText(m map[string]any) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v := m[k]
		if fromTo, ok := v.(map[string]any); ok && len(fromTo) == 2 && fromTo["from"] != nil && fromTo["to"] != nil {
			parts = append(parts, fmt.Sprintf("%s: %v → %v", k, jsonValue(fromTo["from"]), jsonValue(fromTo["to"])))
			continue
		}
		parts = append(parts, k+": "+jsonValue(v))
	}
	return strings.Join(parts, "、")
}

// jsonValue は値を表示用の文字列にする（文字列はそのまま、それ以外は JSON）。
func jsonValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
