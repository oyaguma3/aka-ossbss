package db

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
)

func TestTenantManagement(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	meta := Meta{Operator: "d@example.com", TraceID: "t1"}
	a, err := d.CreateTenant(ctx, "テナントA", meta)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.CreateTenant(ctx, "テナントB", meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateTenant(ctx, "テナントA", meta); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := d.RenameTenant(ctx, b.ID, "テナントA", meta); !errors.Is(err, ErrConflict) {
		t.Errorf("rename to duplicate = %v", err)
	}
	if before, err := d.RenameTenant(ctx, b.ID, "テナント甲", meta); err != nil || before != "テナントB" {
		t.Fatalf("rename = %q, %v", before, err)
	}
	// 名前が変わらなければ状態の事実を書かない。
	if _, err := d.RenameTenant(ctx, b.ID, "テナント甲", meta); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RenameTenant(ctx, 999, "x", meta); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing = %v", err)
	}
	// 件数つきの一覧（台帳の加入者は削除済みを数えない）。
	register(t, d, "001010000000001", a.ID)
	register(t, d, "001010000000002", a.ID)
	if err := d.WithSubscriber(ctx, "001010000000002", meta, func(l Ledger) error {
		return l.Apply(Change{Kind: ChangeDelete, Side: SideVecgen})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAccount(ctx, "u@example.com", authz.TenantUser, a.ID, "d@example.com"); err != nil {
		t.Fatal(err)
	}
	list, err := d.TenantSummaries(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "テナントA" || list[0].Subscribers != 1 || list[0].Accounts != 1 ||
		list[1].Name != "テナント甲" || list[1].Subscribers != 0 || list[1].CreatedAt.IsZero() {
		t.Errorf("summaries = %+v, %v", list, err)
	}
	if s, err := d.TenantSummary(ctx, a.ID); err != nil || s.Accounts != 1 {
		t.Errorf("summary = %+v, %v", s, err)
	}
	if _, err := d.TenantSummary(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("summary missing = %v", err)
	}
	// 属するものが残っていれば削除できない。
	if err := d.DeleteTenant(ctx, a.ID, meta); !errors.Is(err, ErrInUse) {
		t.Errorf("delete in use = %v", err)
	}
	if err := d.DeleteTenant(ctx, b.ID, meta); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteTenant(ctx, b.ID, meta); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing = %v", err)
	}
	// 削除したテナントは一覧に出さず、ID は使い回さない。名前は使い直せる。
	if l, _ := d.Tenants(ctx); len(l) != 1 || l[0].ID != a.ID {
		t.Errorf("tenants after delete = %+v", l)
	}
	if _, err := d.TenantSummary(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("summary of deleted = %v", err)
	}
	c, err := d.CreateTenant(ctx, "テナント甲", meta)
	if err != nil || c.ID <= b.ID {
		t.Errorf("recreate = %+v, %v", c, err)
	}
	// 削除したテナントには加入者を属させない。
	err = d.WithSubscriber(ctx, "001010000000009", meta, func(l Ledger) error {
		return l.Apply(Change{Kind: ChangeRegister, Side: SideVecgen, TenantID: b.ID, State: StateActive})
	})
	if !errors.Is(err, ErrTenantNotFound) {
		t.Errorf("register to deleted tenant = %v", err)
	}

	// テナントの状態の事実（作成 3、名前の変更 1、削除 1）。
	evs, err := d.ChargingEvents(ctx, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range evs {
		if ev.IMSI == "" {
			got = append(got, fmt.Sprintf("%s %d %s→%s %s %s", ev.Event, ev.TenantID, ev.From, ev.To, ev.Operator, ev.TraceID))
		}
	}
	want := []string{
		fmt.Sprintf("tenant_created %d →テナントA d@example.com t1", a.ID),
		fmt.Sprintf("tenant_created %d →テナントB d@example.com t1", b.ID),
		fmt.Sprintf("tenant_updated %d テナントB→テナント甲 d@example.com t1", b.ID),
		fmt.Sprintf("tenant_deleted %d テナント甲→ d@example.com t1", b.ID),
		fmt.Sprintf("tenant_created %d →テナント甲 d@example.com t1", c.ID),
	}
	if !slices.Equal(got, want) {
		t.Errorf("tenant events =\n%v\nwant\n%v", got, want)
	}
}

// register は IMSI を vecgen 側に active で登録する（試験の準備）。
func register(t *testing.T, d *DB, imsi string, tenant int64) {
	t.Helper()
	if err := d.WithSubscriber(t.Context(), imsi, Meta{Operator: "d@example.com"}, func(l Ledger) error {
		return l.Apply(Change{Kind: ChangeRegister, Side: SideVecgen, TenantID: tenant, State: StateActive})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAccountManagement(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ta, _ := d.CreateTenant(ctx, "テナントA", Meta{})
	tb, _ := d.CreateTenant(ctx, "テナントB", Meta{})
	uid, err := d.CreateAccount(ctx, "u@example.com", authz.TenantUser, ta.ID, "b@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAccount(ctx, "c@example.com", authz.Operator, 0, "d@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAccount(ctx, "u@example.com", authz.Operator, 0, "d"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate = %v", err)
	}
	// 役割とテナントの組み合わせは CHECK 制約でも守る。
	if _, err := d.CreateAccount(ctx, "x@example.com", authz.TenantUser, 0, "d"); err == nil {
		t.Error("tenant user without tenant was created")
	}
	if _, err := d.CreateAccount(ctx, "y@example.com", authz.TenantUser, 999, "d"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown tenant = %v", err)
	}
	u, err := d.AccountByID(ctx, uid)
	if err != nil || u.Email != "u@example.com" || u.TenantName != "テナントA" || u.CreatedBy != "b@example.com" || u.CreatedAt.IsZero() {
		t.Errorf("by id = %+v, %v", u, err)
	}
	if _, err := d.AccountByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
	all, _ := d.Accounts(ctx, AccountFilter{All: true})
	none, _ := d.Accounts(ctx, AccountFilter{NoTenant: true})
	inA, _ := d.Accounts(ctx, AccountFilter{TenantID: ta.ID})
	emails := func(l []Account) []string {
		var out []string
		for _, a := range l {
			out = append(out, a.Email)
		}
		return out
	}
	if !slices.Equal(emails(all), []string{"c@example.com", "u@example.com"}) || !slices.Equal(emails(none), []string{"c@example.com"}) ||
		!slices.Equal(emails(inA), []string{"u@example.com"}) {
		t.Errorf("accounts = %v / %v / %v", emails(all), emails(none), emails(inA))
	}

	cur := AccountState{Role: authz.TenantUser, TenantID: ta.ID}
	// 画面を開いた後に変わっていたら何もしない。
	if err := d.UpdateAccount(ctx, uid, AccountState{Role: authz.TenantAdmin, TenantID: ta.ID}, AccountState{Role: authz.Operator}); !errors.Is(err, ErrNotFound) {
		t.Errorf("stale update = %v", err)
	}
	next := AccountState{Role: authz.TenantAdmin, TenantID: tb.ID}
	if err := d.UpdateAccount(ctx, uid, cur, next); err != nil {
		t.Fatal(err)
	}
	if u, _ := d.AccountByID(ctx, uid); u.Role != authz.TenantAdmin || u.TenantID != tb.ID {
		t.Errorf("updated = %+v", u)
	}
	// 運用担当にするとテナントは NULL。
	op := AccountState{Role: authz.Operator}
	if err := d.UpdateAccount(ctx, uid, next, op); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAccountDisabled(ctx, uid, next, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("stale disable = %v", err)
	}
	if err := d.SetAccountDisabled(ctx, uid, op, true); err != nil {
		t.Fatal(err)
	}
	if u, _ := d.AccountByID(ctx, uid); !u.Disabled || u.TenantID != 0 {
		t.Errorf("disabled = %+v", u)
	}
	if err := d.DeleteAccount(ctx, uid, cur); !errors.Is(err, ErrNotFound) {
		t.Errorf("stale delete = %v", err)
	}
	if err := d.DeleteAccount(ctx, uid, op); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AccountByID(ctx, uid); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted = %v", err)
	}
}

func TestAuditList(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ta, _ := d.CreateTenant(ctx, "テナントA", Meta{})

	// 監査ログの読み出し（新しい順、テナント・操作・続きの位置での絞り込み）。
	for i, e := range []AuditEntry{
		{ActorEmail: "c@example.com", ActorRole: authz.Operator, TenantID: ta.ID, Action: "subscriber.create", Target: "001010000000001", Detail: map[string]any{"amf": "8000"}},
		{ActorEmail: "c@example.com", ActorRole: authz.Operator, Action: "client.create", Target: "#3", JobID: "j1"},
		{ActorEmail: "a@example.com", ActorRole: authz.TenantUser, TenantID: ta.ID, Action: "policy.suspend", Target: "001010000000001", TraceID: "t3"},
	} {
		if id, at, err := d.InsertAudit(ctx, e); err != nil || id < 1 || at.IsZero() {
			t.Fatalf("%d: %d %v %v", i, id, at, err)
		}
	}
	all, err := d.ListAudit(ctx, AuditQuery{AllTenants: true, Limit: 10})
	if err != nil || len(all) != 3 || all[0].Action != "policy.suspend" || all[0].TraceID != "t3" || all[0].ActorRole != authz.TenantUser ||
		all[2].Detail["amf"] != "8000" || all[1].TenantID != 0 || all[1].Detail == nil || all[1].JobID != "j1" {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if l, _ := d.ListAudit(ctx, AuditQuery{TenantID: ta.ID, Limit: 10}); len(l) != 2 {
		t.Errorf("tenant = %+v", l)
	}
	if l, _ := d.ListAudit(ctx, AuditQuery{NoTenant: true, Limit: 10}); len(l) != 1 || l[0].Action != "client.create" {
		t.Errorf("no tenant = %+v", l)
	}
	if l, _ := d.ListAudit(ctx, AuditQuery{AllTenants: true, Action: "subscriber.create", Limit: 10}); len(l) != 1 {
		t.Errorf("action = %+v", l)
	}
	if l, _ := d.ListAudit(ctx, AuditQuery{AllTenants: true, Before: all[0].ID, Limit: 1}); len(l) != 1 || l[0].ID != all[1].ID {
		t.Errorf("before = %+v", l)
	}
}
