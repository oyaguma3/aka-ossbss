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

	// PoC のスキーマ（所属の記録 subscriber_tenants）の DB では起動しない（段階 3-1 の W2）。
	if _, err := d.Pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;
		CREATE TABLE schema_migrations (version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO schema_migrations (version, name) VALUES (1, 'init');
		CREATE TABLE subscriber_tenants (imsi text PRIMARY KEY, tenant_id bigint NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Migrate(ctx); !errors.Is(err, ErrPoCSchema) {
		t.Errorf("PoC schema: %v", err)
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

	// 台帳の形式と、有無と状態・取得先の組み合わせ。
	addSub := func(imsi string, vp bool, vs any, rp bool, rs, src any, deleted bool) error {
		var del any
		if deleted {
			del = "2026-10-30T00:00:00Z"
		}
		return exec(`INSERT INTO subscribers (imsi, tenant_id, vecgen_present, vecgen_state, radius_present, radius_state, radius_source_id, deleted_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, imsi, tenant, vp, vs, rp, rs, src, del)
	}
	for _, c := range []struct {
		imsi    string
		vp      bool
		vs      any
		rp      bool
		rs, src any
		deleted bool
		code    string
	}{
		{"001010000000001", true, "active", true, "inactive", "vecgen", false, ""},
		{"001010000000002", false, nil, false, nil, nil, true, ""},  // 削除済み
		{"001010000000003", false, nil, false, nil, nil, false, ""}, // 登録の結果を確かめている
		{"00101000000000x", true, "active", false, nil, nil, false, checkViolation},
		{"001010000000004", true, nil, false, nil, nil, false, checkViolation},         // ある側には状態が要る
		{"001010000000005", false, "active", false, nil, nil, false, checkViolation},   // ない側に状態はない
		{"001010000000006", false, nil, true, "active", nil, false, checkViolation},    // radius 側には取得先が要る
		{"001010000000007", true, "suspended", false, nil, nil, false, checkViolation}, // 未知の状態
		{"001010000000008", true, "active", false, nil, nil, true, checkViolation},     // 削除済みにはどちらの側もない
		{"001010000000001", false, nil, false, nil, nil, true, uniqueViolation},
	} {
		if got := pgCode(addSub(c.imsi, c.vp, c.vs, c.rp, c.rs, c.src, c.deleted)); got != c.code {
			t.Errorf("subscriber %s: code %q, want %q", c.imsi, got, c.code)
		}
	}
	if code := pgCode(exec("INSERT INTO subscribers (imsi, tenant_id) VALUES ('001010000000009', 999999)")); code != fkViolation {
		t.Errorf("unknown tenant: %q", code)
	}
	// 状態の事実の種類と、回線・側の有無の組み合わせ。
	for _, c := range []struct {
		event      string
		side, imsi any
		code       string
	}{
		{"line_registered", "vecgen", "001010000000001", ""},
		{"line_tenant_moved", nil, "001010000000001", ""},
		{"tenant_created", nil, nil, ""},
		{"line_registered", nil, "001010000000001", checkViolation},
		{"tenant_created", nil, "001010000000001", checkViolation},
		{"line_paused", "vecgen", "001010000000001", checkViolation},
	} {
		if got := pgCode(exec("INSERT INTO charging_events (event, side, tenant_id, imsi) VALUES ($1, $2, $3, $4)", c.event, c.side, tenant, c.imsi)); got != c.code {
			t.Errorf("event %s: code %q, want %q", c.event, got, c.code)
		}
	}

	// 台帳の行・アカウントが残るテナントは物理削除できない（テナントの削除は deleted_at で行うが、念のため）。
	if code := pgCode(exec("DELETE FROM tenants WHERE id = $1", tenant)); code != restrictViolation {
		t.Errorf("delete tenant in use: %q", code)
	}

	// 監査ログは、テナントを外部キーにしない（削除した記録も残す）。detail は JSON。
	if err := exec(`INSERT INTO audit_log (actor_email, actor_role, tenant_id, action, target, detail)
		VALUES ('d@example.com', 'system_admin', 999999, 'tenant.delete', $1, '{"name":"テナントA"}')`, "tenant"); err != nil {
		t.Errorf("audit: %v", err)
	}
}
