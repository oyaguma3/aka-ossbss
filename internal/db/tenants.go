package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// テナントの管理（システム管理者だけ。設計概要 §6）。テナントの作成・変更・削除は、課金レコードの状態の事実
// （charging_events）と同じトランザクションで書く。削除しても行を残し、ID を使い回さない（ID は刻印の値になる）。

var (
	// ErrConflict は一意の値（テナント名、メールアドレス）が既にあることを表す。
	ErrConflict = errors.New("already exists")
	// ErrInUse は、加入者やアカウントが残っていて削除できないことを表す。
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
)

// Tenant はテナント（削除していないもの）。
type Tenant struct {
	ID   int64
	Name string
}

// Tenants は削除していないテナントの一覧を名前の順に返す（選択肢と、名前の表示に使う）。
func (d *DB) Tenants(ctx context.Context) ([]Tenant, error) {
	rows, err := d.Pool.Query(ctx, "SELECT id, name FROM tenants WHERE deleted_at IS NULL ORDER BY name, id")
	if err != nil {
		return nil, fmt.Errorf("tenants: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Tenant])
	if err != nil {
		return nil, fmt.Errorf("tenants: %w", err)
	}
	return out, nil
}

// TenantSummary はテナントと、属するものの件数（画面仕様 §8）。
type TenantSummary struct {
	Tenant
	ExternalCustomerID string
	// Subscribers は台帳の加入者（削除済みを除く）、Accounts はアカウントの件数。
	Subscribers, Accounts int64
	CreatedAt             time.Time
}

const tenantSummarySelect = `
	SELECT t.id, t.name, t.external_customer_id,
		(SELECT count(*) FROM subscribers s WHERE s.tenant_id = t.id AND s.deleted_at IS NULL),
		(SELECT count(*) FROM accounts a WHERE a.tenant_id = t.id),
		t.created_at
	FROM tenants t WHERE t.deleted_at IS NULL`

func scanTenantSummary(row pgx.Row) (TenantSummary, error) {
	var s TenantSummary
	err := row.Scan(&s.ID, &s.Name, &s.ExternalCustomerID, &s.Subscribers, &s.Accounts, &s.CreatedAt)
	return s, err
}

// TenantSummaries は削除していないテナントの一覧を、属するものの件数つきで名前の順に返す。
func (d *DB) TenantSummaries(ctx context.Context) ([]TenantSummary, error) {
	rows, err := d.Pool.Query(ctx, tenantSummarySelect+" ORDER BY t.name, t.id")
	if err != nil {
		return nil, fmt.Errorf("tenant summaries: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TenantSummary, error) { return scanTenantSummary(row) })
	if err != nil {
		return nil, fmt.Errorf("tenant summaries: %w", err)
	}
	return out, nil
}

// TenantSummary は 1 つのテナントを件数つきで返す。ない・削除済みなら ErrNotFound。
func (d *DB) TenantSummary(ctx context.Context, id int64) (TenantSummary, error) {
	s, err := scanTenantSummary(d.Pool.QueryRow(ctx, tenantSummarySelect+" AND t.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantSummary{}, ErrNotFound
	}
	if err != nil {
		return TenantSummary{}, fmt.Errorf("tenant summary: %w", err)
	}
	return s, nil
}

// tenantEvent はテナントの状態の事実を書く（時刻はトランザクションの時刻）。
func tenantEvent(ctx context.Context, tx pgx.Tx, meta Meta, event string, id int64, from, to string) error {
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return err
	}
	return insertChargingEvents(ctx, tx, []ChargingEvent{{Time: now, Event: event, TenantID: id, From: from, To: to,
		Operator: meta.Operator, JobID: meta.JobID, TraceID: meta.TraceID}})
}

// CreateTenant はテナントを作る。名前が既にあれば ErrConflict。
func (d *DB) CreateTenant(ctx context.Context, name string, meta Meta) (Tenant, error) {
	t := Tenant{Name: name}
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "INSERT INTO tenants (name) VALUES ($1) RETURNING id", name).Scan(&t.ID); err != nil {
			return err
		}
		return tenantEvent(ctx, tx, meta, EventTenantCreated, t.ID, "", name)
	})
	if pgErrorCode(err) == pgUniqueViolation {
		return Tenant{}, ErrConflict
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("create tenant: %w", err)
	}
	return t, nil
}

// RenameTenant はテナントの名前を変え、変える前の名前を返す。ない・削除済みなら ErrNotFound、名前が既にあれば ErrConflict。
// 名前が変わらなければ何も書かない。
func (d *DB) RenameTenant(ctx context.Context, id int64, name string, meta Meta) (string, error) {
	var before string
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "SELECT name FROM tenants WHERE id = $1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&before)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || before == name {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE tenants SET name = $2, updated_at = now() WHERE id = $1", id, name); err != nil {
			return err
		}
		return tenantEvent(ctx, tx, meta, EventTenantUpdated, id, before, name)
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return "", ErrNotFound
	case pgErrorCode(err) == pgUniqueViolation:
		return "", ErrConflict
	case err != nil:
		return "", fmt.Errorf("rename tenant: %w", err)
	}
	return before, nil
}

// SetTenantExternalID はテナントの外部の顧客番号を変え、変える前の値を返す（状態の事実は tenant_updated。理由で名前の変更と見分ける。Z15）。
// ない・削除済みなら ErrNotFound。値が変わらなければ何も書かない。
func (d *DB) SetTenantExternalID(ctx context.Context, id int64, value string, meta Meta) (string, error) {
	var before string
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "SELECT external_customer_id FROM tenants WHERE id = $1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&before)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || before == value {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, "UPDATE tenants SET external_customer_id = $2, updated_at = now() WHERE id = $1 RETURNING now()", id, value).Scan(&now); err != nil {
			return err
		}
		return insertChargingEvents(ctx, tx, []ChargingEvent{{Time: now, Event: EventTenantUpdated, TenantID: id, From: before, To: value,
			Operator: meta.Operator, JobID: meta.JobID, TraceID: meta.TraceID, Reason: TenantExternalReason}})
	})
	if errors.Is(err, ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("set tenant external id: %w", err)
	}
	return before, nil
}

// DeleteTenant はテナントを削除済みにする（行は残す）。ない・削除済みなら ErrNotFound、加入者（削除済みを除く）や
// アカウントが残っていれば ErrInUse。
func (d *DB) DeleteTenant(ctx context.Context, id int64, meta Meta) error {
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var name string
		err := tx.QueryRow(ctx, "SELECT name FROM tenants WHERE id = $1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// 加入者とアカウントを足すときの外部キーの確かめは、テナントの行の共有ロックを取るので、この行のロック
		// （FOR UPDATE）と順になる。ここで数えれば、数えた後に足されることはない。
		var inUse bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subscribers WHERE tenant_id = $1 AND deleted_at IS NULL)
			OR EXISTS (SELECT 1 FROM accounts WHERE tenant_id = $1)`, id).Scan(&inUse); err != nil {
			return err
		}
		if inUse {
			return ErrInUse
		}
		if _, err := tx.Exec(ctx, "UPDATE tenants SET deleted_at = now(), updated_at = now() WHERE id = $1", id); err != nil {
			return err
		}
		return tenantEvent(ctx, tx, meta, EventTenantDeleted, id, name, "")
	})
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrInUse):
		return err
	case err != nil:
		return fmt.Errorf("delete tenant: %w", err)
	}
	return nil
}
