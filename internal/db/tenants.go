package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// テナントの管理（システム管理者だけ。設計概要 §7、画面仕様 §8）。

var (
	// ErrConflict は一意の値（テナント名、メールアドレス）が既にあることを表す。
	ErrConflict = errors.New("already exists")
	// ErrInUse は、所属の記録やアカウントが残っていて削除できないことを表す（外部キーの ON DELETE RESTRICT）。
	ErrInUse = errors.New("in use")
)

// pgErrorCode は PostgreSQL のエラーの SQLSTATE を返す（PostgreSQL のエラーでなければ空）。
func pgErrorCode(err error) string {
	if e, ok := errors.AsType[*pgconn.PgError](err); ok {
		return e.Code
	}
	return ""
}

const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	// pgRestrictViolation は ON DELETE RESTRICT の外部キーで削除を断ったときの SQLSTATE（NO ACTION なら 23503）。
	pgRestrictViolation = "23001"
)

// TenantSummary はテナントと、属するものの件数（画面仕様 §8）。
type TenantSummary struct {
	Tenant
	// Subscribers と Clients は所属の記録の件数、Accounts はアカウントの件数。
	Subscribers, Clients, Accounts int64
	CreatedAt                      time.Time
}

// TenantSummaries はテナントの一覧を、属するものの件数つきで名前の順に返す。
func (d *DB) TenantSummaries(ctx context.Context) ([]TenantSummary, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT t.id, t.name,
			(SELECT count(*) FROM subscriber_tenants s WHERE s.tenant_id = t.id),
			(SELECT count(*) FROM client_tenants c WHERE c.tenant_id = t.id),
			(SELECT count(*) FROM accounts a WHERE a.tenant_id = t.id),
			t.created_at
		FROM tenants t ORDER BY t.name, t.id`)
	if err != nil {
		return nil, fmt.Errorf("tenant summaries: %w", err)
	}
	var out []TenantSummary
	var s TenantSummary
	if _, err := pgx.ForEachRow(rows, []any{&s.ID, &s.Name, &s.Subscribers, &s.Clients, &s.Accounts, &s.CreatedAt}, func() error {
		out = append(out, s)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("tenant summaries: %w", err)
	}
	return out, nil
}

// TenantSummary は 1 つのテナントを件数つきで返す。なければ ErrNotFound。
func (d *DB) TenantSummary(ctx context.Context, id int64) (TenantSummary, error) {
	var s TenantSummary
	err := d.Pool.QueryRow(ctx, `
		SELECT t.id, t.name,
			(SELECT count(*) FROM subscriber_tenants s WHERE s.tenant_id = t.id),
			(SELECT count(*) FROM client_tenants c WHERE c.tenant_id = t.id),
			(SELECT count(*) FROM accounts a WHERE a.tenant_id = t.id),
			t.created_at
		FROM tenants t WHERE t.id = $1`, id).Scan(&s.ID, &s.Name, &s.Subscribers, &s.Clients, &s.Accounts, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantSummary{}, ErrNotFound
	}
	if err != nil {
		return TenantSummary{}, fmt.Errorf("tenant summary: %w", err)
	}
	return s, nil
}

// CreateTenant はテナントを作る。名前が既にあれば ErrConflict。
func (d *DB) CreateTenant(ctx context.Context, name string) (Tenant, error) {
	t := Tenant{Name: name}
	err := d.Pool.QueryRow(ctx, "INSERT INTO tenants (name) VALUES ($1) RETURNING id", name).Scan(&t.ID)
	if pgErrorCode(err) == pgUniqueViolation {
		return Tenant{}, ErrConflict
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("create tenant: %w", err)
	}
	return t, nil
}

// RenameTenant はテナントの名前を変える。なければ ErrNotFound、名前が既にあれば ErrConflict。
func (d *DB) RenameTenant(ctx context.Context, id int64, name string) error {
	tag, err := d.Pool.Exec(ctx, "UPDATE tenants SET name = $2, updated_at = now() WHERE id = $1", id, name)
	switch {
	case pgErrorCode(err) == pgUniqueViolation:
		return ErrConflict
	case err != nil:
		return fmt.Errorf("rename tenant: %w", err)
	case tag.RowsAffected() == 0:
		return ErrNotFound
	}
	return nil
}

// DeleteTenant はテナントを削除する。なければ ErrNotFound、所属の記録やアカウントが残っていれば ErrInUse。
func (d *DB) DeleteTenant(ctx context.Context, id int64) error {
	tag, err := d.Pool.Exec(ctx, "DELETE FROM tenants WHERE id = $1", id)
	switch {
	case pgErrorCode(err) == pgRestrictViolation || pgErrorCode(err) == pgForeignKeyViolation:
		return ErrInUse
	case err != nil:
		return fmt.Errorf("delete tenant: %w", err)
	case tag.RowsAffected() == 0:
		return ErrNotFound
	}
	return nil
}
