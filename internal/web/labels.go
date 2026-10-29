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

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// 画面に出す名前と、値の表示の形。

// stateLabels は加入者の状態の名前。
var stateLabels = map[string]string{
	db.StateActive:     "利用中",
	db.StateInactive:   "停止中",
	db.StateTerminated: "解約",
	db.FilterAbsent:    "なし",
}

// stateLabel は加入者の状態の名前を返す（空は「なし」）。
func stateLabel(s string) string {
	if s == "" {
		return stateLabels[db.FilterAbsent]
	}
	return cmp.Or(stateLabels[s], s)
}

// sideLabel は加入者のデータの側の名前を返す。
func sideLabel(s db.Side) string {
	switch s {
	case db.SideVecgen:
		return "vecgen 側（aka-vecgen）"
	case db.SideRadius:
		return "radius 側（aka-radius）"
	}
	return "下流"
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

// auditActions は aka-ossbss の監査ログの操作と名前（画面の操作の種類の絞り込みの順）。
var auditActions = []struct{ Action, Label string }{
	{auditVecgenCreate, "vecgen 側の登録"},
	{auditRadiusCreate, "radius 側の登録"},
	{auditReregister, "削除済みの IMSI の登録し直しの承認"},
	{auditVecgenUpdate, "vecgen 側の変更"},
	{auditSourceUpdate, "取得先の変更"},
	{auditPolicyUpdate, "認可ポリシーの変更"},
	{auditStateChange, "状態の変更"},
	{auditReinstate, "解約からの戻し"},
	{auditSubscriberDelete, "物理削除"},
	{auditTenantMove, "所属のテナントの付け替え"},
	{auditCheck, "結果の確かめ（台帳の補い）"},
	{auditTenantCreate, "テナントの作成"},
	{auditTenantUpdate, "テナントの名前の変更"},
	{auditTenantDelete, "テナントの削除"},
	{auditAccountCreate, "アカウントの作成"},
	{auditAccountUpdate, "アカウントの変更"},
	{auditAccountDisable, "アカウントの無効化"},
	{auditAccountEnable, "アカウントの有効化"},
	{auditAccountDelete, "アカウントの削除"},
}

// otherActionLabels は、aka-provisioner と下流の監査ログにだけ出る操作の名前。
var otherActionLabels = map[string]string{
	"subscriber.create":    "加入者の登録",
	"subscriber.update":    "加入者の変更",
	"subscriber.delete":    "加入者の削除",
	"subscriber.state":     "状態の変更",
	"subscriber.reinstate": "解約からの戻し",
	"subscriber.keys.read": "Ki / OPc の読み出し",
	"reload":               "設定の読み直し",
}

// actionLabel は監査ログの操作の名前を返す。aka-provisioner の「<下流>.<口>」（例 vecgen.subscriber.create）は、
// 下流の名前を添える。
func actionLabel(action string) string {
	for _, a := range auditActions {
		if a.Action == action {
			return a.Label
		}
	}
	if l, ok := otherActionLabels[action]; ok {
		return l
	}
	if d, rest, ok := strings.Cut(action, "."); ok && (d == "vecgen" || d == "radius") {
		if l, ok := otherActionLabels[rest]; ok {
			return downstreamLabel(d) + ": " + l
		}
	}
	return action
}

// knownAction は、aka-ossbss の監査ログの操作の名前か。
func knownAction(action string) bool {
	return slices.ContainsFunc(auditActions, func(a struct{ Action, Label string }) bool { return a.Action == action })
}

// detailText は監査ログの内容（JSON のオブジェクト）を、項目の名前の順に「名前: 値」で並べた 1 行にする。
// 下流の変更の { "from": …, "to": … } は「変更前 → 変更後」にする。
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

// formatID は番号を 10 進の文字列にする。
func formatID(id int64) string { return strconv.FormatInt(id, 10) }
