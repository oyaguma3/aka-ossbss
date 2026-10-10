package web

import (
	"cmp"
	"strconv"
	"time"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
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
	provisioner.IssueAVClientNotAllowed: "aka-only-server の加入者が、本PoCの vector-gateway の AVクライアントを許可していないため、認証できません。" +
		"aka-only-server の管理 GUI などで、許可するクライアントに加えてください。",
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
		return "aka-only-server"
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
