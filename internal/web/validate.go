package web

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

// 入力の形式（provisioner と下流の検証、本PoCの pkg/validation と同じ）。provisioner に送る前に本BFF でも確かめ、誤りを日本語で示す。
var (
	imsiPattern       = regexp.MustCompile(`^[0-9]{15}$`)
	prefixDigits      = regexp.MustCompile(`^[0-9]{1,15}$`)
	hex128            = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sqnPattern        = regexp.MustCompile(`^[0-9a-f]{12}$`)
	amfPattern        = regexp.MustCompile(`^[0-9a-f]{4}$`)
	clientNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	vendorPattern     = regexp.MustCompile(`^[A-Za-z0-9 -]{0,64}$`)
	secretPattern     = regexp.MustCompile(`^[\x21-\x7E]{1,128}$`)
	nasIDPattern      = regexp.MustCompile(`^[\x21-\x7E]{1,253}$`)
	vlanIDPattern     = regexp.MustCompile(`^[0-9]{1,4}$`)
)

// 認可ポリシーの上限（本PoCの pkg/validation と同じ）。
const (
	maxSSIDLen        = 32
	maxVLANID         = 4094
	maxSessionTimeout = 86400
)

// maxFormBytes はフォームの大きさの上限（認可ポリシーのルール 100 件でも収まる大きさ）。
const maxFormBytes = 1 << 20

// parseForm はフォームを読む（大きさに上限を設ける）。
func parseForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	return r.ParseForm()
}

// validIPv4 は、IPv4 アドレスとして正しく、ドット区切りの 10 進で先頭に 0 のない表記かを返す。
func validIPv4(v string) bool {
	a, err := netip.ParseAddr(v)
	return err == nil && a.Is4() && a.String() == v
}

// fieldErrors は項目ごとの入力の誤り。
type fieldErrors map[string]string

func (f fieldErrors) check(ok bool, field, msg string) {
	if !ok {
		if _, dup := f[field]; !dup {
			f[field] = msg
		}
	}
}

// normalizeHex は 16 進の入力から前後の空白を除き、小文字にする。
func normalizeHex(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// fieldLabels は provisioner の項目名を画面の名前にする。
var fieldLabels = map[string]string{
	"imsi":       "IMSI",
	"ki":         "Ki",
	"opc":        "OPc",
	"sqn":        "SQN",
	"amf":        "AMF",
	"sqnType":    "SQN の増加タイプ",
	"allowPlain": "平文HTTP の許可",
	"ip":         "IP アドレス",
	"secret":     "共有シークレット",
	"name":       "名前",
	"vendor":     "ベンダー",
	"default":    "既定の動作",
	"rules":      "ルール",
}

// ruleParamPattern は認可ポリシーのルールの項目名（例: rules[0].allowedSsids[1]）。
var ruleParamPattern = regexp.MustCompile(`^(?:policy\.)?rules\[(\d+)\]\.(nasId|allowedSsids|vlanId|sessionTimeout)(?:\[(\d+)\])?$`)

// ruleFieldLabels はルールの項目名を画面の名前にする。
var ruleFieldLabels = map[string]string{
	"nasId":          "NAS-ID",
	"allowedSsids":   "許可する SSID",
	"vlanId":         "VLAN ID",
	"sessionTimeout": "Session-Timeout",
}

// paramLabel は invalidParams の項目名を画面の名前にする。
func paramLabel(p string) string {
	if m := ruleParamPattern.FindStringSubmatch(p); m != nil {
		i, _ := strconv.Atoi(m[1])
		return fmt.Sprintf("ルール %d の %s", i+1, ruleFieldLabels[m[2]])
	}
	return cmp.Or(fieldLabels[strings.TrimPrefix(p, "policy.")], p)
}

// apiFailure は provisioner の呼び出しの失敗を、画面に出す形にしたもの。
type apiFailure struct {
	Status  int
	Message string
	// OperationID は provisioner の操作の記録の ID（途中で失敗した操作、または同じ IMSI の未完了の操作）。
	// 操作の記録を見られる運用担当・システム管理者にだけ出す。
	OperationID string
	// Unreached は provisioner に届かなかった（接続できない・タイムアウト）こと。フォームの Idempotency-Key を残すのに使う。
	Unreached bool
}

// 画面の文に使う言い回し。
const (
	elsewhere           = "他の操作（本PoCの Admin TUI など）で変更された可能性があります。"
	registeredElsewhere = "（本PoCの Admin TUI など、他の操作で登録された場合もあります）。"
	// retryLater は、テナントの利用者（A・B）に、自分では対処できない失敗のときに添える文。
	retryLater = "しばらくしてからもう一度操作してください。続く場合は運用担当に連絡してください。"
)

// notFoundMessage は、provisioner に対象がないときの説明を作る。
func notFoundMessage(what string) string {
	return what + " は登録されていません。他の操作（本PoCの Admin TUI など）で削除された可能性があります。"
}

// isStaff は、利用者が provisioner の状態・操作の記録を見られる役割（運用担当・システム管理者）か。
// エラーの文で、操作の記録や設定の名前を案内するかを決める（設計概要 §6.2、画面仕様 §1.6）。
func isStaff(r *http.Request) bool {
	return authz.Allowed(userFrom(r.Context()).Actor(), authz.ProvisionerView, authz.NoTenant)
}

// apiError は provisioner の呼び出しのエラーを、画面に出す形にする。notFound は対象がないときの説明（空なら既定の文）。
// テナントの利用者（A・B）には、操作の記録や設定の名前を出さない（画面仕様 §1.6）。
func (h *Handler) apiError(r *http.Request, err error, notFound string) apiFailure {
	staff := isStaff(r)
	f := apiErrorFor(err, notFound, staff)
	if !staff {
		f.OperationID = ""
	}
	return f
}

func apiErrorFor(err error, notFound string, staff bool) apiFailure {
	if provisioner.IsUnavailable(err) {
		if !staff {
			return apiFailure{Status: http.StatusBadGateway, Message: "provisioner に接続できません。" + retryLater, Unreached: true}
		}
		return apiFailure{Status: http.StatusBadGateway, Message: cmp.Or(provisioner.Diagnose(err), "provisioner に接続できません。"), Unreached: true}
	}
	apiErr, ok := errors.AsType[*provisioner.Error](err)
	if !ok {
		return apiFailure{Status: http.StatusInternalServerError, Message: "処理に失敗しました。"}
	}
	p := apiErr.Problem
	// rolledBack は、書き込みの途中で失敗し、補償で元に戻したことを示す（このときは operationId も付く）。
	var rolledBack string
	if p.RolledBack != nil && *p.RolledBack {
		rolledBack = "途中まで行った変更は元に戻しました。"
	}
	switch p.Cause {
	case provisioner.CauseUserNotFound:
		return apiFailure{Status: http.StatusNotFound, Message: cmp.Or(notFound, "加入者が見つかりません。"+elsewhere)}
	case provisioner.CauseClientNotFound:
		return apiFailure{Status: http.StatusNotFound, Message: cmp.Or(notFound, "RADIUSクライアントが見つかりません。"+elsewhere)}
	case provisioner.CausePolicyNotFound:
		return apiFailure{Status: http.StatusNotFound, Message: cmp.Or(notFound, "認可ポリシーが見つかりません。"+elsewhere)}
	case provisioner.CauseSubscriberExists:
		msg := "その IMSI の加入者は既に登録されています" + registeredElsewhere
		if places := conflictPlaces(p.Conflicts); places != "" {
			msg = "その IMSI は既に登録されています（" + places + "）" + registeredElsewhere
		}
		return apiFailure{Status: http.StatusConflict, Message: msg}
	case provisioner.CauseClientExists:
		return apiFailure{Status: http.StatusConflict, Message: "その IP アドレスの RADIUSクライアントは既に登録されています" + registeredElsewhere}
	case provisioner.CauseOperationInProgress:
		return apiFailure{Status: http.StatusConflict, Message: "同じ IMSI の操作が処理中です。少し待ってから、もう一度操作してください。"}
	case provisioner.CauseOperationUnresolved:
		if !staff {
			return apiFailure{Status: http.StatusConflict, Message: "同じ IMSI に完了していない操作が残っています。運用担当に対応を依頼してください。"}
		}
		return apiFailure{Status: http.StatusConflict, OperationID: p.OperationID,
			Message: "同じ IMSI に完了していない操作が残っています。「操作の記録」で、その操作をやり直すか閉じてから、もう一度操作してください。"}
	case provisioner.CauseOperationIncomplete:
		if !staff {
			return apiFailure{Status: http.StatusInternalServerError,
				Message: downstreamLabel(p.Downstream) + " への操作が途中で失敗しました。provisioner が後で自動でやり直します。続く場合は運用担当に連絡してください。"}
		}
		return apiFailure{Status: http.StatusInternalServerError, OperationID: p.OperationID,
			Message: downstreamLabel(p.Downstream) + " への操作が途中で失敗し、元に戻せませんでした。provisioner が後で自動でやり直します。「操作の記録」で状況を確かめてください。"}
	case provisioner.CauseDownstreamUnavailable:
		msg := "provisioner から" + spaceBeforeASCII(downstreamLabel(p.Downstream)) + " に接続できません。" + rolledBack
		if !staff {
			return apiFailure{Status: http.StatusServiceUnavailable, Message: msg + retryLater}
		}
		return apiFailure{Status: http.StatusServiceUnavailable, OperationID: p.OperationID, Message: msg + "ダッシュボードで接続の状態を確かめてください。"}
	case provisioner.CauseDownstreamError:
		return apiFailure{Status: http.StatusBadGateway, OperationID: p.OperationID,
			Message: downstreamLabel(p.Downstream) + " がエラーを返しました（" + cmp.Or(p.DownstreamCause, strconv.Itoa(p.DownstreamStatus)) + "）。" + rolledBack}
	case provisioner.CauseKeyStoreMismatch:
		return apiFailure{Status: http.StatusBadRequest, Message: "鍵の置き場所が、provisioner の PLMN マップから決まるものと違います。"}
	case provisioner.CauseIdempotencyKeyMismatch:
		return apiFailure{Status: http.StatusConflict, Message: "送信の内容が前回と違います。画面を開き直してから、もう一度操作してください。"}
	case provisioner.CauseDownstreamNotConfigured:
		return apiFailure{Status: http.StatusNotFound, Message: "provisioner は aka-only-server を扱わない設定です。"}
	}
	if apiErr.Status == http.StatusBadRequest && len(p.InvalidParams) > 0 {
		var names []string
		for _, ip := range p.InvalidParams {
			names = append(names, paramLabel(ip.Param))
		}
		return apiFailure{Status: http.StatusBadRequest, Message: "入力が正しくありません（" + strings.Join(names, "、") + "）。"}
	}
	if apiErr.Status == http.StatusNotFound {
		return apiFailure{Status: http.StatusNotFound, Message: cmp.Or(notFound, "対象が見つかりません。")}
	}
	return apiFailure{Status: http.StatusBadGateway, Message: "provisioner がエラーを返しました。"}
}

// downstreamLabels は provisioner の下流の名前。
var downstreamLabels = map[string]string{"prov": "本PoCの Provisioning API", "aka": "aka-only-server"}

// downstreamLabel は provisioner の下流（prov / aka）の画面の名前を返す。
func downstreamLabel(name string) string { return cmp.Or(downstreamLabels[name], name) }

// spaceBeforeASCII は、英数字で始まる語の前に空白を入れる（「provisioner から aka-only-server」「provisioner から本PoC」）。
func spaceBeforeASCII(s string) string {
	if s != "" && s[0] < 0x80 {
		return " " + s
	}
	return s
}

// conflictPlaces は、provisioner が SUBSCRIBER_ALREADY_EXISTS で返した場所（poc / aka / policy）を画面の言葉にする。
func conflictPlaces(places []string) string {
	labels := map[string]string{"poc": "本PoCの鍵", "aka": "aka-only-server の鍵", "policy": "認可ポリシー"}
	var out []string
	for _, p := range places {
		out = append(out, cmp.Or(labels[p], p))
	}
	return strings.Join(out, "、")
}
