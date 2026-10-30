package web

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 入力の形式（下流の検証と同じ）。aka-provisioner に送る前に aka-ossbss でも確かめ、誤りを日本語で示す。
var (
	imsiPattern     = regexp.MustCompile(`^[0-9]{15}$`)
	prefixDigits    = regexp.MustCompile(`^[0-9]{1,15}$`)
	hex128          = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sqnPattern      = regexp.MustCompile(`^[0-9a-f]{12}$`)
	amfPattern      = regexp.MustCompile(`^[0-9a-f]{4}$`)
	sourceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// cursorPattern はセッションの一覧の続きの位置（aka-radius の nextCursor）。
	cursorPattern = regexp.MustCompile(`^[0-9]{1,9}$`)
	// nasIDPattern は NAS-Identifier（制御文字を除く 1〜253 文字。* はすべて）。
	nasIDPattern = regexp.MustCompile(`^[^\x00-\x1F\x7F]{1,253}$`)
)

// 認可ポリシーの上限（aka-radius の管理API 2.0.0 と同じ）。
const (
	maxRules          = 32
	maxSSIDs          = 32
	maxSSIDLen        = 32
	maxVLANID         = 4094
	maxSessionTimeout = 86400
	// maxReasonLen は状態の変更・戻しの理由の文字数の上限（下流と同じ）。
	maxReasonLen = 500
)

// sqnTypes は SQN の方式（aka-vecgen の sqnType）。
var sqnTypes = []string{"inc1", "inc32", "inc33"}

// maxFormBytes はフォームの大きさの上限（認可ポリシーのルール 32 件でも十分に収まる大きさ）。
const maxFormBytes = 1 << 20

// parseForm はフォームを読む（大きさに上限を設ける）。
func parseForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	return r.ParseForm()
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

// fieldLabels は下流の項目名を画面の名前にする。
var fieldLabels = map[string]string{
	"imsi":             "IMSI",
	"ki":               "Ki",
	"opc":              "OPc",
	"sqn":              "SQN",
	"amf":              "AMF",
	"sqnType":          "SQN の方式",
	"state":            "状態",
	"reason":           "理由",
	"allowedClientIds": "許可クライアント",
	"sourceId":         "取得先",
	"rules":            "ルール",
}

// ruleParamPattern は認可ポリシーのルールの項目名（例: rules[0].ssids[1]）。
var ruleParamPattern = regexp.MustCompile(`^rules\[(\d+)\]\.(tenantId|nasId|ssids|vlanId|sessionTimeout)(?:\[(\d+)\])?$`)

// ruleFieldLabels はルールの項目名を画面の名前にする。
var ruleFieldLabels = map[string]string{
	"tenantId":       "テナント",
	"nasId":          "NAS-Identifier",
	"ssids":          "SSID",
	"vlanId":         "VLAN",
	"sessionTimeout": "Session-Timeout",
}

// paramLabel は invalidParams の項目名を画面の名前にする。
func paramLabel(p string) string {
	if m := ruleParamPattern.FindStringSubmatch(p); m != nil {
		i, _ := strconv.Atoi(m[1])
		return fmt.Sprintf("ルール %d の %s", i+1, ruleFieldLabels[m[2]])
	}
	return cmp.Or(fieldLabels[p], p)
}

// apiFailure は aka-provisioner の呼び出しの失敗を、画面に出す形にしたもの。
type apiFailure struct {
	Status  int
	Message string
}

// retryLater は、テナントの利用者（A・B）に、自分では対処できない失敗のときに添える文。
const retryLater = "しばらくしてからもう一度操作してください。続く場合は運用担当に連絡してください。"

// downstreamLabel は下流の画面の名前を返す。
func downstreamLabel(name string) string {
	switch name {
	case string(provisioner.Vecgen):
		return "aka-vecgen"
	case string(provisioner.Radius):
		return "aka-radius"
	}
	return cmp.Or(name, "下流")
}

// apiError は aka-provisioner の呼び出しの誤りを、画面に出す形にする。side はその操作の側（文に使う。空でもよい）。
// テナントの利用者（A・B）には、設定の名前や接続の見当を出さない。
func (h *Handler) apiError(r *http.Request, err error, side db.Side) apiFailure {
	return apiErrorFor(err, side, isStaff(r))
}

// DescribeProvisionerError は aka-provisioner の呼び出しの誤りを、運用者に見せる文にする（一括投入の結果の行に使う）。
func DescribeProvisionerError(err error, side db.Side) string {
	return apiErrorFor(err, side, true).Message
}

func apiErrorFor(err error, side db.Side, staff bool) apiFailure {
	where := sideLabel(side)
	if provisioner.IsUnavailable(err) {
		if !staff {
			return apiFailure{http.StatusBadGateway, "aka-provisioner に接続できません。" + retryLater}
		}
		return apiFailure{http.StatusBadGateway, cmp.Or(provisioner.Diagnose(err), "aka-provisioner に接続できません。")}
	}
	apiErr, ok := errors.AsType[*provisioner.Error](err)
	if !ok {
		return apiFailure{http.StatusInternalServerError, "処理に失敗しました。"}
	}
	p := apiErr.Problem
	if p.Downstream != "" {
		where = downstreamLabel(p.Downstream)
	}
	switch p.Cause {
	case provisioner.CauseUserNotFound:
		return apiFailure{http.StatusNotFound, where + "にこの加入者がありません（aka-ossbss の外で削除された可能性があります。台帳は突き合わせで直します）。"}
	case provisioner.CauseSubscriberExists:
		return apiFailure{http.StatusConflict, where + "には、この IMSI の加入者が既に登録されています（台帳にない場合は、aka-ossbss の外で登録されたものです。システム管理者に連絡してください）。"}
	case provisioner.CauseSubscriberActive:
		return apiFailure{http.StatusConflict, "利用中（Active）の加入者は物理削除できません。先に止めるか解約してください。"}
	case provisioner.CauseSubscriberTerminated:
		return apiFailure{http.StatusConflict, "解約（Terminated）の加入者は変更できません。システム管理者が戻してから変更してください。"}
	case provisioner.CauseStateTransitionNotAllowed:
		return apiFailure{http.StatusConflict, "その状態には変えられません（解約からは、システム管理者が「戻す」で停止中に戻します）。画面を開き直してください。"}
	case provisioner.CauseClientNotFound:
		return apiFailure{http.StatusBadRequest, "選んだ許可クライアントが aka-vecgen にありません。画面を開き直してください。"}
	case provisioner.CauseUnknownSource:
		return apiFailure{http.StatusBadRequest, "選んだ取得先が aka-radius の登録簿にありません。画面を開き直してください。"}
	case provisioner.CauseOperationInProgress:
		return apiFailure{http.StatusConflict, "同じ操作が処理中です。少し待ってから、画面を開き直して確かめてください。"}
	case provisioner.CauseIdempotencyKeyMismatch:
		return apiFailure{http.StatusConflict, "送信の内容が前回と違います。画面を開き直してから、もう一度操作してください。"}
	case provisioner.CauseDownstreamUnavailable:
		msg := "aka-provisioner から " + where + " に接続できません。"
		if !staff {
			return apiFailure{http.StatusServiceUnavailable, msg + retryLater}
		}
		return apiFailure{http.StatusServiceUnavailable, msg + "ダッシュボードで接続の状態を確かめてください。"}
	case provisioner.CauseDownstreamError:
		return apiFailure{http.StatusBadGateway, where + " が不正な応答を返しました（" + strconv.Itoa(p.DownstreamStatus) + "）。"}
	case provisioner.CauseRouteNotAllowed:
		return apiFailure{http.StatusBadGateway, "aka-provisioner がこの口を通しませんでした（aka-provisioner の版を確かめてください）。"}
	}
	if apiErr.Status == http.StatusBadRequest && len(p.InvalidParams) > 0 {
		var names []string
		for _, ip := range p.InvalidParams {
			names = append(names, paramLabel(ip.Param))
		}
		return apiFailure{http.StatusBadRequest, "入力が正しくありません（" + strings.Join(names, "、") + "）。"}
	}
	if apiErr.Status == http.StatusNotFound {
		return apiFailure{http.StatusNotFound, "対象が見つかりません。"}
	}
	if apiErr.Status >= 500 && !staff {
		return apiFailure{http.StatusBadGateway, where + " で誤りが起きました。" + retryLater}
	}
	return apiFailure{http.StatusBadGateway, where + " が誤りを返しました（" + strconv.Itoa(apiErr.Status) + " " + cmp.Or(p.Cause, "-") + "）。"}
}

// parseID はフォームの番号（1 以上）を読む。
func parseID(v string) (int64, error) {
	id, err := strconv.ParseInt(v, 10, 64)
	if err == nil && id < 1 {
		err = errors.New("must be positive")
	}
	return id, err
}
