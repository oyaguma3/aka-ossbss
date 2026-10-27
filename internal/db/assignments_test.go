package db

import (
	"maps"
	"slices"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
)

func TestAssignments(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var ta, tb int64
	if err := d.Pool.QueryRow(ctx, "INSERT INTO tenants (name) VALUES ('テナントB') RETURNING id").Scan(&tb); err != nil {
		t.Fatal(err)
	}
	if err := d.Pool.QueryRow(ctx, "INSERT INTO tenants (name) VALUES ('テナントA') RETURNING id").Scan(&ta); err != nil {
		t.Fatal(err)
	}
	// テナントの一覧は名前の順。
	tenants, err := d.Tenants(ctx)
	if err != nil || !slices.Equal(tenants, []Tenant{{ta, "テナントA"}, {tb, "テナントB"}}) {
		t.Errorf("tenants = %v, %v", tenants, err)
	}

	// 加入者の所属。
	for _, imsi := range []string{"001010000000003", "001010000000001", "001020000000002"} {
		if err := d.AssignSubscriber(ctx, imsi, ta, "c@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AssignSubscriber(ctx, "001010000000009", tb, "c@example.com"); err != nil {
		t.Fatal(err)
	}
	// 付け替え（upsert）。
	if err := d.AssignSubscriber(ctx, "001020000000002", tb, "d@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.SubscriberTenant(ctx, "001020000000002"); err != nil || got != tb {
		t.Errorf("reassigned = %d, %v", got, err)
	}
	// 記録がなければ 0（テナントなし）。
	if got, err := d.SubscriberTenant(ctx, "001019999999999"); err != nil || got != 0 {
		t.Errorf("unassigned = %d, %v", got, err)
	}
	// テナントの範囲の一覧（IMSI の順、after より後ろ、前方一致）。
	if got, err := d.TenantSubscribers(ctx, ta, "", "", 10); err != nil || !slices.Equal(got, []string{"001010000000001", "001010000000003"}) {
		t.Errorf("tenant subscribers = %v, %v", got, err)
	}
	if got, err := d.TenantSubscribers(ctx, ta, "", "001010000000001", 10); err != nil || !slices.Equal(got, []string{"001010000000003"}) {
		t.Errorf("after = %v, %v", got, err)
	}
	if got, err := d.TenantSubscribers(ctx, ta, "", "", 1); err != nil || len(got) != 1 {
		t.Errorf("limit = %v, %v", got, err)
	}
	if got, err := d.TenantSubscribers(ctx, tb, "00102", "", 10); err != nil || !slices.Equal(got, []string{"001020000000002"}) {
		t.Errorf("prefix = %v, %v", got, err)
	}
	if n, err := d.CountTenantSubscribers(ctx, tb, ""); err != nil || n != 2 {
		t.Errorf("count = %d, %v", n, err)
	}
	if n, err := d.CountTenantSubscribers(ctx, tb, "00102"); err != nil || n != 1 {
		t.Errorf("count prefix = %d, %v", n, err)
	}
	// 「%」「_」は前方一致の中でそのまま扱う（ワイルドカードにしない）。
	if n, err := d.CountTenantSubscribers(ctx, ta, "_"); err != nil || n != 0 {
		t.Errorf("count wildcard = %d, %v", n, err)
	}
	m, err := d.SubscriberTenants(ctx, []string{"001010000000001", "001020000000002", "001019999999999"})
	if err != nil || !maps.Equal(m, map[string]int64{"001010000000001": ta, "001020000000002": tb}) {
		t.Errorf("subscriber tenants = %v, %v", m, err)
	}
	if m, err := d.SubscriberTenants(ctx, nil); err != nil || len(m) != 0 {
		t.Errorf("subscriber tenants (empty) = %v, %v", m, err)
	}
	// テナントなしにすると記録を消す。
	if err := d.AssignSubscriber(ctx, "001010000000003", 0, "c@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.SubscriberTenant(ctx, "001010000000003"); err != nil || got != 0 {
		t.Errorf("unassign = %d, %v", got, err)
	}
	var by string
	if err := d.Pool.QueryRow(ctx, "SELECT assigned_by FROM subscriber_tenants WHERE imsi = '001020000000002'").Scan(&by); err != nil || by != "d@example.com" {
		t.Errorf("assigned_by = %q, %v", by, err)
	}

	// RADIUSクライアントの所属。
	if err := d.AssignClient(ctx, 5, ta, "b@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := d.AssignClient(ctx, 7, ta, "c@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := d.AssignClient(ctx, 7, tb, "d@example.com"); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ClientTenant(ctx, 7); err != nil || got != tb {
		t.Errorf("client tenant = %d, %v", got, err)
	}
	if got, err := d.ClientTenant(ctx, 8); err != nil || got != 0 {
		t.Errorf("client unassigned = %d, %v", got, err)
	}
	if m, err := d.ClientTenants(ctx); err != nil || !maps.Equal(m, map[int64]int64{5: ta, 7: tb}) {
		t.Errorf("client tenants = %v, %v", m, err)
	}
	if err := d.AssignClient(ctx, 5, 0, "d@example.com"); err != nil {
		t.Fatal(err)
	}
	if m, err := d.ClientTenants(ctx); err != nil || !maps.Equal(m, map[int64]int64{7: tb}) {
		t.Errorf("client tenants after unassign = %v, %v", m, err)
	}

	// 所属が残るテナントは削除できない（ON DELETE RESTRICT）。
	if _, err := d.Pool.Exec(ctx, "DELETE FROM tenants WHERE id = $1", tb); err == nil {
		t.Error("tenant with assignments was deleted")
	}
}

func TestInsertAudit(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertAudit(ctx, AuditEntry{ActorEmail: "b@example.com", ActorRole: authz.TenantAdmin, TenantID: 3,
		Action: "policy.suspend", Target: "001010000000001", TraceID: "t-1", Remote: "203.0.113.7"}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertAudit(ctx, AuditEntry{ActorEmail: "c@example.com", ActorRole: authz.Operator,
		Action: "client.update", Target: "#5", Detail: map[string]any{"fields": []string{"ip"}, "ip": "192.0.2.1 -> 192.0.2.2"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Pool.Query(ctx, "SELECT actor_email, actor_role, tenant_id, action, target, detail::text, trace_id, remote FROM audit_log ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		email, role string
		tenant      *int64
		action      string
		target      string
		detail      string
		trace       string
		remote      string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.email, &r.role, &r.tenant, &r.action, &r.target, &r.detail, &r.trace, &r.remote); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d", len(got))
	}
	if r := got[0]; r.email != "b@example.com" || r.role != "tenant_admin" || r.tenant == nil || *r.tenant != 3 ||
		r.action != "policy.suspend" || r.detail != "{}" || r.trace != "t-1" || r.remote != "203.0.113.7" {
		t.Errorf("row 0 = %+v", r)
	}
	// テナントなしは NULL。内容は JSONB。
	if r := got[1]; r.tenant != nil || r.detail != `{"ip": "192.0.2.1 -> 192.0.2.2", "fields": ["ip"]}` {
		t.Errorf("row 1 = %+v (tenant %v)", r, r.tenant)
	}
}
