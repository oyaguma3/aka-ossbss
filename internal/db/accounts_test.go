package db

import (
	"errors"
	"testing"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
)

func TestAccountByEmail(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var tenant int64
	if err := d.Pool.QueryRow(ctx, "INSERT INTO tenants (name) VALUES ('テナントA') RETURNING id").Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO accounts (email, role, tenant_id, disabled) VALUES
		('a@example.com', 'tenant_user', $1, false), ('c@example.com', 'operator', NULL, true)`, tenant); err != nil {
		t.Fatal(err)
	}

	a, err := d.AccountByEmail(ctx, "a@example.com")
	if err != nil || a.Role != authz.TenantUser || a.TenantID != tenant || a.TenantName != "テナントA" || a.Disabled {
		t.Errorf("a = %+v, %v", a, err)
	}
	if got := a.Actor(); got != (authz.Actor{Email: "a@example.com", Role: authz.TenantUser, TenantID: tenant}) {
		t.Errorf("actor = %+v", got)
	}
	c, err := d.AccountByEmail(ctx, "c@example.com")
	if err != nil || c.Role != authz.Operator || c.TenantID != 0 || c.TenantName != "" || !c.Disabled {
		t.Errorf("c = %+v, %v", c, err)
	}
	if _, err := d.AccountByEmail(ctx, "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestEnsureInitialAdmin(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const email = "root@example.com"
	check := func(want InitialAdminResult) {
		t.Helper()
		got, err := d.EnsureInitialAdmin(ctx, email)
		if err != nil || got != want {
			t.Fatalf("ensure = %q, %v; want %q", got, err, want)
		}
		a, err := d.AccountByEmail(ctx, email)
		if err != nil || a.Role != authz.SystemAdmin || a.TenantID != 0 || a.Disabled {
			t.Fatalf("account = %+v, %v", a, err)
		}
	}
	check(InitialAdminCreated)
	check(InitialAdminUnchanged)

	// 無効にされた、または別の役割（テナントつき）に変えられていたら戻す。
	if _, err := d.Pool.Exec(ctx, "UPDATE accounts SET disabled = true WHERE email = $1", email); err != nil {
		t.Fatal(err)
	}
	check(InitialAdminRestored)
	var tenant int64
	if err := d.Pool.QueryRow(ctx, "INSERT INTO tenants (name) VALUES ('T') RETURNING id").Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, "UPDATE accounts SET role = 'tenant_admin', tenant_id = $2 WHERE email = $1", email, tenant); err != nil {
		t.Fatal(err)
	}
	check(InitialAdminRestored)

	if _, err := d.EnsureInitialAdmin(ctx, "Root+x@example.com"); err == nil {
		t.Error("invalid email accepted")
	}
}
