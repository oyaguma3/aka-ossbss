// Package authz は利用者の役割と、操作の権限の判定（設計概要 §5）。副作用を持たない。
//
// 権限の判定は aka-ossbss だけが行う（aka-provisioner は aka-ossbss を全権の管理クライアントとして扱う）。画面に出さないだけでなく、
// 要求を受けたときにも必ず Allowed で判定する（テナントの範囲の確認を含む）。
package authz

import (
	"regexp"
	"slices"
	"strings"
)

// Role は利用者の役割（DB の accounts.role の値）。
type Role string

const (
	// TenantUser（A）は自分のテナントの加入者を見て、状態を変える。
	TenantUser Role = "tenant_user"
	// TenantAdmin（B）は A に加え、自分のテナントの認可ポリシー・テナントユーザー・監査ログを扱う。
	TenantAdmin Role = "tenant_admin"
	// Operator（C）は全テナントの、日常の運用をする。
	Operator Role = "operator"
	// SystemAdmin（D）はすべての操作をする。
	SystemAdmin Role = "system_admin"
)

// Roles は役割の一覧（画面の選択肢の順）。
var Roles = []Role{TenantUser, TenantAdmin, Operator, SystemAdmin}

// roleLabels は画面での役割の呼び名。
var roleLabels = map[Role]string{
	TenantUser:  "テナントユーザー",
	TenantAdmin: "テナント管理者",
	Operator:    "運用担当",
	SystemAdmin: "システム管理者",
}

// Label は役割の呼び名を返す。
func (r Role) Label() string {
	if l, ok := roleLabels[r]; ok {
		return l
	}
	return string(r)
}

// Valid は知っている役割か。
func (r Role) Valid() bool {
	_, ok := roleLabels[r]
	return ok
}

// TenantScoped は、1 つのテナントに属する役割（A・B）か。属する役割は、自分のテナントのものだけを扱う。
func (r Role) TenantScoped() bool { return r == TenantUser || r == TenantAdmin }

// Actor は操作する利用者。
type Actor struct {
	Email string
	Role  Role
	// TenantID は所属のテナント（A・B だけ。C・D は 0）。
	TenantID int64
}

// NoTenant は、テナントに属さないこと（C・D の利用者と、テナントに属さない監査ログ）を表す対象のテナント。
// 加入者は必ずどれかのテナントに属する（設計概要 §6。決定事項 2）。
const NoTenant int64 = 0

// Op は操作（設計概要 §5 の表の行）。
type Op int

const (
	SubscriberView        Op = iota + 1 // 加入者の閲覧（台帳、両側の状態、取得先、認可ポリシー）
	SubscriberDetailView                // AMF・SQN・許可クライアントの閲覧
	SubscriberStateChange               // 状態の変更（Active ⇔ Inactive、Terminated へ）
	SubscriberReinstate                 // Terminated から Inactive に戻す（理由が必須）
	VecgenRegister                      // vecgen 側の個別の登録（鍵を入力）
	RadiusRegister                      // radius 側の個別の登録（取得先と認可ポリシー）
	VecgenUpdate                        // vecgen 側の変更（AMF・SQN・方式・許可クライアント）
	PolicyUpdate                        // 認可ポリシーの変更
	SourceUpdate                        // 取得先の変更
	SubscriberDelete                    // 個別の物理削除（Active を除く）
	ReregisterApprove                   // 削除済みの IMSI の登録し直しの承認
	TenantMove                          // 所属のテナントの付け替え
	SessionView                         // セッションの閲覧
	AuditView                           // aka-ossbss の監査ログの閲覧（B は自分のテナントの分）
	DownstreamAuditView                 // aka-provisioner と下流の監査ログの閲覧
	ProvisionerView                     // aka-provisioner と下流の状態の閲覧
	TenantManage                        // テナントの作成・変更・削除
	AccountManage                       // アカウントの管理（B は自分のテナントの A だけ。CanManageAccount で対象を判定する）
	JobView                             // 一括投入のジョブの閲覧
	JobManage                           // 一括投入・一括の物理削除の登録・投入・中断
	ReconcileView                       // 突き合わせの結果の閲覧
	ReconcileManage                     // 突き合わせの手動の実行と対応
)

// rule は 1 つの操作の権限。
type rule struct {
	label string
	// roles はできる役割。
	roles []Role
	// scoped は、対象のテナントで範囲を絞る操作か（A・B は自分のテナントのものだけ）。
	scoped bool
}

var all = []Role{TenantUser, TenantAdmin, Operator, SystemAdmin}

var rules = map[Op]rule{
	SubscriberView:        {"加入者の閲覧", all, true},
	SubscriberDetailView:  {"AMF・SQN・許可クライアントの閲覧", []Role{Operator, SystemAdmin}, true},
	SubscriberStateChange: {"加入者の状態の変更", all, true},
	SubscriberReinstate:   {"解約（Terminated）からの戻し", []Role{SystemAdmin}, true},
	VecgenRegister:        {"vecgen 側の登録（鍵の入力）", []Role{SystemAdmin}, true},
	RadiusRegister:        {"radius 側の登録", []Role{Operator, SystemAdmin}, true},
	VecgenUpdate:          {"vecgen 側の変更（AMF・SQN・方式・許可クライアント）", []Role{SystemAdmin}, true},
	PolicyUpdate:          {"認可ポリシーの変更", []Role{TenantAdmin, Operator, SystemAdmin}, true},
	SourceUpdate:          {"取得先の変更", []Role{Operator, SystemAdmin}, true},
	SubscriberDelete:      {"加入者の物理削除", []Role{Operator, SystemAdmin}, true},
	ReregisterApprove:     {"削除済みの IMSI の登録し直しの承認", []Role{SystemAdmin}, false},
	TenantMove:            {"所属のテナントの付け替え", []Role{SystemAdmin}, false},
	SessionView:           {"セッションの閲覧", all, true},
	AuditView:             {"監査ログの閲覧", []Role{TenantAdmin, Operator, SystemAdmin}, true},
	DownstreamAuditView:   {"aka-provisioner と下流の監査ログの閲覧", []Role{Operator, SystemAdmin}, false},
	ProvisionerView:       {"aka-provisioner と下流の状態の閲覧", []Role{Operator, SystemAdmin}, false},
	TenantManage:          {"テナントの管理", []Role{SystemAdmin}, false},
	AccountManage:         {"アカウントの管理", []Role{TenantAdmin, SystemAdmin}, true},
	JobView:               {"一括投入のジョブの閲覧", []Role{Operator, SystemAdmin}, false},
	JobManage:             {"一括投入・一括の物理削除", []Role{SystemAdmin}, false},
	ReconcileView:         {"突き合わせの結果の閲覧", []Role{Operator, SystemAdmin}, false},
	ReconcileManage:       {"突き合わせの実行と対応", []Role{SystemAdmin}, false},
}

// Ops は操作の一覧（画面の要約の順）。
func Ops() []Op {
	out := make([]Op, 0, len(rules))
	for op := SubscriberView; op <= ReconcileManage; op++ {
		out = append(out, op)
	}
	return out
}

// Label は操作の名前を返す。
func (op Op) Label() string { return rules[op].label }

// Scoped は、対象のテナントで範囲を絞る操作か。
func (op Op) Scoped() bool { return rules[op].scoped }

// Allowed は、a が対象のテナント target（テナントに属さないものは NoTenant）のものに op をしてよいかを返す。
// 範囲を絞らない操作（テナントの管理など）では target を見ない。
func Allowed(a Actor, op Op, target int64) bool {
	r, ok := rules[op]
	if !ok || !a.Role.Valid() {
		return false
	}
	if !slices.Contains(r.roles, a.Role) {
		return false
	}
	if r.scoped && a.Role.TenantScoped() {
		// A・B は自分のテナントのものだけ。テナントに属さないものは扱えない。
		return a.TenantID != NoTenant && target == a.TenantID
	}
	return true
}

// CanManageAccount は、a が役割 role・テナント tenant のアカウントを作成・変更・無効化・削除してよいかを返す
// （もとの設計概要 §15 の 11）。D はすべて、B は自分のテナントのテナントユーザーだけ。B は A を B に昇格できない
// （変更後の役割もこの関数で確かめる）。最初のシステム管理者の保護は呼び出し側で行う。
func CanManageAccount(a Actor, role Role, tenant int64) bool {
	if !role.Valid() {
		return false
	}
	switch a.Role {
	case SystemAdmin:
		return true
	case TenantAdmin:
		return role == TenantUser && a.TenantID != NoTenant && tenant == a.TenantID
	}
	return false
}

// emailPattern はアカウントのメールアドレスの形式（小文字にしたもの）。aka-provisioner に X-Operator-Id として
// 渡すので、その形式（^[A-Za-z0-9._@-]{1,64}$）に収まるものに限る（DB の CHECK 制約と同じ）。
var emailPattern = regexp.MustCompile(`^[a-z0-9._-]+@[a-z0-9._-]+$`)

// NormalizeEmail はメールアドレスの前後の空白を除き、小文字にする。
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ValidEmail は、正規化したメールアドレスがアカウントに使える形式か（64 文字まで）。
func ValidEmail(email string) bool { return len(email) <= 64 && emailPattern.MatchString(email) }
