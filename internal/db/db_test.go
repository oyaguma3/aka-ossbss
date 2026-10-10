package db

import (
	"cmp"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 実際の PostgreSQL を使う結合テスト。OSSBSS_TEST_DB_ADDR（と _PASSWORD、_NAME、_USER）を指定したときだけ動く。
// テストのたびに public スキーマを作り直すので、名前に test を含む DB でだけ動かす（誤って本番の DB を消さないため）。

func openTestDB(t *testing.T) *DB {
	t.Helper()
	addr := os.Getenv("OSSBSS_TEST_DB_ADDR")
	if addr == "" {
		t.Skip("OSSBSS_TEST_DB_ADDR is not set")
	}
	name := cmp.Or(os.Getenv("OSSBSS_TEST_DB_NAME"), "ossbss_test")
	if !strings.Contains(name, "test") {
		t.Fatalf("OSSBSS_TEST_DB_NAME %q must contain \"test\" (the tests drop the public schema)", name)
	}
	d, err := Open(t.Context(), Options{Addr: addr, Name: name, User: cmp.Or(os.Getenv("OSSBSS_TEST_DB_USER"), "ossbss"),
		Password: os.Getenv("OSSBSS_TEST_DB_PASSWORD"), MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if _, err := d.Pool.Exec(t.Context(), "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMigrate(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()

	applied, err := d.Migrate(ctx)
	if err != nil || len(applied) == 0 || applied[0] != 1 {
		t.Fatalf("first migrate = %v, %v", applied, err)
	}
	// 2 回目は何もしない。
	if again, err := d.Migrate(ctx); err != nil || len(again) != 0 {
		t.Errorf("second migrate = %v, %v", again, err)
	}
	if v, err := d.SchemaVersion(ctx); err != nil || v != applied[len(applied)-1] {
		t.Errorf("schema version = %d, %v", v, err)
	}

	// 同時に移行しても、二重には適用しない（advisory lock）。
	if _, err := d.Pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 3)
	counts := make(chan int, 3)
	for range 3 {
		go func() {
			a, err := d.Migrate(context.WithoutCancel(ctx))
			errs <- err
			counts <- len(a)
		}()
	}
	total := 0
	for range 3 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
		total += <-counts
	}
	if total != len(applied) {
		t.Errorf("applied %d migrations in total, want %d", total, len(applied))
	}

	// プログラムより新しい版の DB は使わない。
	if _, err := d.Pool.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES (9999, 'future')"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer than this program") {
		t.Errorf("future schema: %v", err)
	}
}

// pgCode は PostgreSQL のエラーの SQLSTATE を返す。
func pgCode(err error) string {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pe.Code
	}
	return ""
}

// 制約: 役割とテナントの組み合わせ、メールアドレスの形式、所属が残るテナントの削除。
func TestSchemaConstraints(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var tenant int64
	if err := d.Pool.QueryRow(ctx, "INSERT INTO tenants (name) VALUES ('テナントA') RETURNING id").Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	const (
		checkViolation  = "23514"
		uniqueViolation = "23505"
		fkViolation     = "23503"
		// ON DELETE RESTRICT の違反は restrict_violation。
		restrictViolation = "23001"
	)
	exec := func(sql string, args ...any) error {
		_, err := d.Pool.Exec(ctx, sql, args...)
		return err
	}
	addAccount := func(email, role string, tenant any) error {
		return exec("INSERT INTO accounts (email, role, tenant_id) VALUES ($1, $2, $3)", email, role, tenant)
	}

	for _, c := range []struct {
		email, role string
		tenant      any
		code        string
	}{
		{"a@example.com", "tenant_user", tenant, ""},
		{"b@example.com", "tenant_admin", tenant, ""},
		{"c@example.com", "operator", nil, ""},
		{"d@example.com", "system_admin", nil, ""},
		{"e@example.com", "tenant_user", nil, checkViolation},                       // A・B はテナントが要る
		{"f@example.com", "operator", tenant, checkViolation},                       // C・D はテナントに属さない
		{"g@example.com", "owner", nil, checkViolation},                             // 未知の役割
		{"A@example.com", "operator", nil, checkViolation},                          // 小文字にしてから入れる
		{"h+1@example.com", "operator", nil, checkViolation},                        // X-Operator-Id に使えない文字
		{"example.com", "operator", nil, checkViolation},                            // @ がない
		{strings.Repeat("x", 53) + "@example.com", "operator", nil, checkViolation}, // 64 文字を超える
		{"a@example.com", "operator", nil, uniqueViolation},
		{"i@example.com", "tenant_user", int64(999999), fkViolation},
	} {
		if got := pgCode(addAccount(c.email, c.role, c.tenant)); got != c.code {
			t.Errorf("account %s %s: code %q, want %q", c.email, c.role, got, c.code)
		}
	}

	// 所属の記録の形式。
	if code := pgCode(exec("INSERT INTO subscriber_tenants (imsi, tenant_id) VALUES ('00101000000000x', $1)", tenant)); code != checkViolation {
		t.Errorf("bad imsi: %q", code)
	}
	if err := exec("INSERT INTO subscriber_tenants (imsi, tenant_id) VALUES ('001010000000001', $1)", tenant); err != nil {
		t.Fatal(err)
	}
	if code := pgCode(exec("INSERT INTO client_tenants (client_id, tenant_id) VALUES (0, $1)", tenant)); code != checkViolation {
		t.Errorf("bad client id: %q", code)
	}

	// 所属・利用者が残るテナントは削除できない。
	if code := pgCode(exec("DELETE FROM tenants WHERE id = $1", tenant)); code != restrictViolation {
		t.Errorf("delete tenant in use: %q", code)
	}
	if err := exec("DELETE FROM subscriber_tenants; DELETE FROM accounts WHERE tenant_id IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
	if err := exec("DELETE FROM tenants WHERE id = $1", tenant); err != nil {
		t.Errorf("delete unused tenant: %v", err)
	}

	// 監査ログは、テナントを消しても残る（外部キーにしない）。detail は JSON。
	if err := exec(`INSERT INTO audit_log (actor_email, actor_role, tenant_id, action, target, detail)
		VALUES ('d@example.com', 'system_admin', $1, 'tenant.delete', $2, '{"name":"テナントA"}')`, tenant, "tenant"); err != nil {
		t.Errorf("audit: %v", err)
	}
}
