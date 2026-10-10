package authz

import (
	"strings"
	"testing"
)

// 設計概要 §6.2 の表を、実装の表とは別に書き写した期待値。
// 列は A（テナントユーザー）・B（テナント管理者）・C（運用担当）・D（システム管理者）。
// "o" はできる、"x" はできない、"t" は自分のテナントのものだけ（テナントなし・他のテナントは不可）。
var table = map[Op]string{
	SubscriberCreate:       "xxoo",
	SubscriberView:         "ttoo",
	SubscriberDetailView:   "xxoo",
	SubscriberKeysView:     "xxoo",
	SubscriberPolicyUpdate: "xtoo",
	SubscriberKeysUpdate:   "xxoo",
	SubscriberAuthUpdate:   "xxxo",
	SubscriberDelete:       "xxoo",
	SubscriberSuspend:      "ttoo",
	ClientCreate:           "xtoo",
	ClientView:             "ttoo",
	ClientUpdate:           "xtoo",
	ClientSecretView:       "xtoo",
	AssignmentUpdate:       "xxxo",
	SessionView:            "ttoo",
	AuditView:              "xtoo",
	DownstreamAuditView:    "xxxo",
	ProvisionerView:        "xxoo",
	OperationManage:        "xxxo",
	TenantManage:           "xxxo",
	AccountManage:          "xtxo",
	OrphanDelete:           "xxxo",
}

func TestAllowedMatrix(t *testing.T) {
	const own, other = 10, 20
	if len(table) != len(Ops()) {
		t.Fatalf("table has %d ops, implementation has %d", len(table), len(Ops()))
	}
	for _, op := range Ops() {
		row, ok := table[op]
		if !ok {
			t.Errorf("op %d (%s) is not in the table", op, op.Label())
			continue
		}
		for i, role := range Roles {
			a := Actor{Email: "x@example.com", Role: role}
			if role.TenantScoped() {
				a.TenantID = own
			}
			for _, target := range []int64{own, other, NoTenant} {
				var want bool
				switch row[i] {
				case 'o':
					want = true
				case 't':
					want = target == own
				}
				if got := Allowed(a, op, target); got != want {
					t.Errorf("%s by %s on tenant %d: got %v, want %v", op.Label(), role.Label(), target, got, want)
				}
			}
		}
	}
}

// A・B なのにテナントがない（DB の制約で起きないはずの）利用者や、未知の役割・操作は、何もできない。
func TestAllowedInvalid(t *testing.T) {
	if Allowed(Actor{Role: TenantAdmin}, SubscriberView, NoTenant) {
		t.Error("tenant admin without tenant can view tenantless subscribers")
	}
	if Allowed(Actor{Role: "owner"}, SubscriberView, NoTenant) {
		t.Error("unknown role is allowed")
	}
	if Allowed(Actor{Role: SystemAdmin}, Op(999), NoTenant) {
		t.Error("unknown op is allowed")
	}
}

func TestCanManageAccount(t *testing.T) {
	const own, other = 10, 20
	b := Actor{Role: TenantAdmin, TenantID: own}
	d := Actor{Role: SystemAdmin}
	for _, c := range []struct {
		a      Actor
		role   Role
		tenant int64
		want   bool
	}{
		{b, TenantUser, own, true},
		{b, TenantUser, other, false},  // 他のテナント
		{b, TenantAdmin, own, false},   // B は B を管理しない（昇格もできない）
		{b, Operator, NoTenant, false}, // C・D は管理しない
		{d, TenantAdmin, other, true},
		{d, SystemAdmin, NoTenant, true},
		{d, "owner", NoTenant, false}, // 未知の役割
		{Actor{Role: Operator}, TenantUser, own, false},
		{Actor{Role: TenantUser, TenantID: own}, TenantUser, own, false},
	} {
		if got := CanManageAccount(c.a, c.role, c.tenant); got != c.want {
			t.Errorf("%s manages %s of tenant %d: got %v, want %v", c.a.Role.Label(), c.role, c.tenant, got, c.want)
		}
	}
}

func TestEmail(t *testing.T) {
	if got := NormalizeEmail("  Alice@Example.COM "); got != "alice@example.com" {
		t.Errorf("normalize = %q", got)
	}
	for _, c := range []struct {
		email string
		want  bool
	}{
		{"alice@example.com", true},
		{"a.b_c-d@sub.example.co.jp", true},
		{"alice+tag@example.com", false}, // X-Operator-Id に使えない
		{"Alice@example.com", false},     // 正規化していない
		{"example.com", false},
		{"@example.com", false},
		{strings.Repeat("a", 53) + "@example.com", false}, // 65 文字
		{strings.Repeat("a", 52) + "@example.com", true},  // 64 文字
	} {
		if got := ValidEmail(c.email); got != c.want {
			t.Errorf("ValidEmail(%q) = %v", c.email, got)
		}
	}
}

func TestRoleLabels(t *testing.T) {
	want := map[Role]string{TenantUser: "テナントユーザー", TenantAdmin: "テナント管理者", Operator: "運用担当", SystemAdmin: "システム管理者"}
	for r, l := range want {
		if r.Label() != l || !r.Valid() {
			t.Errorf("%s: %q", r, r.Label())
		}
	}
	if Role("x").Valid() {
		t.Error("unknown role is valid")
	}
}
