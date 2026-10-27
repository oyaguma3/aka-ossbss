// Package authz は利用者の役割と、操作の権限の判定（設計概要 §6）。副作用を持たない。
//
// 権限の判定は本BFF だけが行う（provisioner は本BFF を全権の管理クライアントとして扱う）。画面に出さないだけでなく、
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
	// TenantUser（A）は自分のテナントの加入者と RADIUSクライアントを見て、加入者の停止・再開をする。
	TenantUser Role = "tenant_user"
	// TenantAdmin（B）は A に加え、自分のテナントの認可ポリシー・RADIUSクライアント・テナントユーザーを管理する。
	TenantAdmin Role = "tenant_admin"
	// Operator（C）は全テナントとテナントなしの、日常の運用をする。
	Operator Role = "operator"
	// SystemAdmin（D）はすべての操作をする。
	SystemAdmin Role = "system_admin"
)

// Roles は役割の一覧（画面の選択肢の順）。
var Roles = []Role{TenantUser, TenantAdmin, Operator, SystemAdmin}

// roleLabels は画面での役割の呼び名（設計概要 §15 の 13）。
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

// NoTenant は「テナントなし」（所属の記録がない加入者・RADIUSクライアント）を表す対象のテナント。
const NoTenant int64 = 0

// Op は操作（設計概要 §6.2 の表の行）。
type Op int

const (
	SubscriberCreate       Op = iota + 1 // 加入者の登録
	SubscriberView                       // 加入者の閲覧（IMSI・認可ポリシー・停止の状態）
	SubscriberDetailView                 // 加入者の鍵の置き場所・AMF・SQN・食い違いの閲覧
	SubscriberKeysView                   // 加入者の Ki / OPc の表示
	SubscriberPolicyUpdate               // 加入者の変更: 認可ポリシー
	SubscriberKeysUpdate                 // 加入者の変更: Ki / OPc
	SubscriberAuthUpdate                 // 加入者の変更: AMF / SQN（aka-vecgen の加入者は SQN の増加タイプ・平文HTTP の許可も）
	SubscriberDelete                     // 加入者の削除
	SubscriberSuspend                    // 加入者の停止・再開
	ClientCreate                         // RADIUSクライアントの登録
	ClientView                           // RADIUSクライアントの閲覧（共有シークレットを除く）
	ClientUpdate                         // RADIUSクライアントの変更・削除
	ClientSecretView                     // RADIUSクライアントの共有シークレットの表示
	AssignmentUpdate                     // 所属のテナントの付け替え・外す
	SessionView                          // セッションの閲覧
	AuditView                            // 本BFF の監査ログの閲覧（B は自分のテナントの分）
	DownstreamAuditView                  // provisioner と下流の監査ログの閲覧（設計概要 §13）
	ProvisionerView                      // provisioner の状態・操作の記録の閲覧
	OperationManage                      // 操作の記録のやり直し・閉じる
	TenantManage                         // テナントの作成・変更・削除
	AccountManage                        // アカウントの管理（B は自分のテナントの A だけ。CanManageAccount で対象を判定する）
	OrphanDelete                         // 下流にない所属の記録の削除
)

// rule は 1 つの操作の権限。
type rule struct {
	label string
	// roles はできる役割。
	roles []Role
	// scoped は、対象のテナントで範囲を絞る操作か（A・B は自分のテナントのものだけ。テナントなしは C・D だけ）。
	scoped bool
}

var rules = map[Op]rule{
	SubscriberCreate:       {"加入者の登録", []Role{Operator, SystemAdmin}, true},
	SubscriberView:         {"加入者の閲覧", []Role{TenantUser, TenantAdmin, Operator, SystemAdmin}, true},
	SubscriberDetailView:   {"鍵の置き場所・AMF・SQN・食い違いの閲覧", []Role{Operator, SystemAdmin}, true},
	SubscriberKeysView:     {"Ki / OPc の表示", []Role{Operator, SystemAdmin}, true},
	SubscriberPolicyUpdate: {"認可ポリシーの変更", []Role{TenantAdmin, Operator, SystemAdmin}, true},
	SubscriberKeysUpdate:   {"Ki / OPc の変更", []Role{Operator, SystemAdmin}, true},
	SubscriberAuthUpdate:   {"AMF / SQN の変更", []Role{SystemAdmin}, true},
	SubscriberDelete:       {"加入者の削除", []Role{Operator, SystemAdmin}, true},
	SubscriberSuspend:      {"加入者の停止・再開", []Role{TenantUser, TenantAdmin, Operator, SystemAdmin}, true},
	ClientCreate:           {"RADIUSクライアントの登録", []Role{TenantAdmin, Operator, SystemAdmin}, true},
	ClientView:             {"RADIUSクライアントの閲覧", []Role{TenantUser, TenantAdmin, Operator, SystemAdmin}, true},
	ClientUpdate:           {"RADIUSクライアントの変更・削除", []Role{TenantAdmin, Operator, SystemAdmin}, true},
	ClientSecretView:       {"共有シークレットの表示", []Role{TenantAdmin, Operator, SystemAdmin}, true},
	AssignmentUpdate:       {"所属のテナントの付け替え", []Role{SystemAdmin}, false},
	SessionView:            {"セッションの閲覧", []Role{TenantUser, TenantAdmin, Operator, SystemAdmin}, true},
	AuditView:              {"監査ログの閲覧", []Role{TenantAdmin, Operator, SystemAdmin}, true},
	DownstreamAuditView:    {"provisioner と下流の監査ログの閲覧", []Role{SystemAdmin}, false},
	ProvisionerView:        {"provisioner の状態・操作の記録の閲覧", []Role{Operator, SystemAdmin}, false},
	OperationManage:        {"操作の記録のやり直し・閉じる", []Role{SystemAdmin}, false},
	TenantManage:           {"テナントの管理", []Role{SystemAdmin}, false},
	AccountManage:          {"アカウントの管理", []Role{TenantAdmin, SystemAdmin}, true},
	OrphanDelete:           {"下流にない所属の記録の削除", []Role{SystemAdmin}, false},
}

// Ops は操作の一覧（画面の要約の順）。
func Ops() []Op {
	out := make([]Op, 0, len(rules))
	for op := SubscriberCreate; op <= OrphanDelete; op++ {
		out = append(out, op)
	}
	return out
}

// Label は操作の名前を返す。
func (op Op) Label() string { return rules[op].label }

// Scoped は、対象のテナントで範囲を絞る操作か。
func (op Op) Scoped() bool { return rules[op].scoped }

// Allowed は、a が対象のテナント target（テナントなしは NoTenant）のものに op をしてよいかを返す。
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
		// A・B は自分のテナントのものだけ。テナントなしは扱えない。
		return a.TenantID != NoTenant && target == a.TenantID
	}
	return true
}

// CanManageAccount は、a が役割 role・テナント tenant のアカウントを作成・変更・無効化・削除してよいかを返す
// （設計概要 §15 の 11）。D はすべて、B は自分のテナントのテナントユーザーだけ。B は A を B に昇格できない
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

// emailPattern はアカウントのメールアドレスの形式（小文字にしたもの）。provisioner に X-Operator-Id として
// 渡すので、その形式（^[A-Za-z0-9._@-]{1,64}$）に収まるものに限る（設計概要 §15 の 6。DB の CHECK 制約と同じ）。
var emailPattern = regexp.MustCompile(`^[a-z0-9._-]+@[a-z0-9._-]+$`)

// NormalizeEmail はメールアドレスの前後の空白を除き、小文字にする。
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ValidEmail は、正規化したメールアドレスがアカウントに使える形式か（64 文字まで）。
func ValidEmail(email string) bool { return len(email) <= 64 && emailPattern.MatchString(email) }
