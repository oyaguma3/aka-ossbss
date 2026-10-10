package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// テナントと、加入者・RADIUSクライアントの所属の記録（設計概要 §7、§11）。
// 所属の記録がなければ「テナントなし」。テナントの ID の 0 は「テナントなし」を表す（authz.NoTenant と同じ）。

// Tenant はテナント。
type Tenant struct {
	ID   int64
	Name string
}

// Tenants はテナントの一覧を名前の順に返す（登録のときの選択肢と、名前の表示に使う）。
func (d *DB) Tenants(ctx context.Context) ([]Tenant, error) {
	rows, err := d.Pool.Query(ctx, "SELECT id, name FROM tenants ORDER BY name, id")
	if err != nil {
		return nil, fmt.Errorf("tenants: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Tenant])
	if err != nil {
		return nil, fmt.Errorf("tenants: %w", err)
	}
	return out, nil
}

// SubscriberTenant は IMSI の所属のテナントを返す（記録がなければ 0）。
func (d *DB) SubscriberTenant(ctx context.Context, imsi string) (int64, error) {
	var id int64
	err := d.Pool.QueryRow(ctx, "SELECT tenant_id FROM subscriber_tenants WHERE imsi = $1", imsi).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("subscriber tenant: %w", err)
	}
	return id, nil
}

// SubscriberTenants は、IMSI の一覧のうち所属の記録があるものについて、IMSI → テナントを返す。
func (d *DB) SubscriberTenants(ctx context.Context, imsis []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(imsis) == 0 {
		return out, nil
	}
	rows, err := d.Pool.Query(ctx, "SELECT imsi, tenant_id FROM subscriber_tenants WHERE imsi = ANY($1)", imsis)
	if err != nil {
		return nil, fmt.Errorf("subscriber tenants: %w", err)
	}
	var imsi string
	var id int64
	if _, err := pgx.ForEachRow(rows, []any{&imsi, &id}, func() error {
		out[imsi] = id
		return nil
	}); err != nil {
		return nil, fmt.Errorf("subscriber tenants: %w", err)
	}
	return out, nil
}

// TenantSubscribers は、テナントに属する IMSI を IMSI の順に、after より後ろから limit 件まで返す
// （テナントの範囲の一覧。設計概要 §7.1）。prefix が空でなければ、その前方一致に絞る。
func (d *DB) TenantSubscribers(ctx context.Context, tenantID int64, prefix, after string, limit int) ([]string, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT imsi FROM subscriber_tenants
		WHERE tenant_id = $1 AND imsi LIKE $2 AND imsi > $3
		ORDER BY imsi LIMIT $4`, tenantID, likePrefix(prefix), after, limit)
	if err != nil {
		return nil, fmt.Errorf("tenant subscribers: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("tenant subscribers: %w", err)
	}
	return out, nil
}

// CountTenantSubscribers は、テナントに属する IMSI の数を返す（prefix は TenantSubscribers と同じ）。
func (d *DB) CountTenantSubscribers(ctx context.Context, tenantID int64, prefix string) (int64, error) {
	var n int64
	err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM subscriber_tenants WHERE tenant_id = $1 AND imsi LIKE $2",
		tenantID, likePrefix(prefix)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count tenant subscribers: %w", err)
	}
	return n, nil
}

// likePrefix は前方一致の LIKE のパターンを作る（IMSI の前方一致は数字だけなので、エスケープの要る文字は含まないが念のため除く）。
func likePrefix(prefix string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(prefix) + "%"
}

// AssignSubscriber は IMSI の所属を tenantID にする（by は操作者）。tenantID が 0 なら記録を消す（テナントなし）。
func (d *DB) AssignSubscriber(ctx context.Context, imsi string, tenantID int64, by string) error {
	var err error
	if tenantID == 0 {
		_, err = d.Pool.Exec(ctx, "DELETE FROM subscriber_tenants WHERE imsi = $1", imsi)
	} else {
		_, err = d.Pool.Exec(ctx, `
			INSERT INTO subscriber_tenants (imsi, tenant_id, assigned_by) VALUES ($1, $2, $3)
			ON CONFLICT (imsi) DO UPDATE SET tenant_id = EXCLUDED.tenant_id, assigned_at = now(), assigned_by = EXCLUDED.assigned_by`,
			imsi, tenantID, by)
	}
	if err != nil {
		return fmt.Errorf("assign subscriber: %w", err)
	}
	return nil
}

// ClientTenant は RADIUSクライアントの所属のテナントを返す（記録がなければ 0）。
func (d *DB) ClientTenant(ctx context.Context, clientID int64) (int64, error) {
	var id int64
	err := d.Pool.QueryRow(ctx, "SELECT tenant_id FROM client_tenants WHERE client_id = $1", clientID).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("client tenant: %w", err)
	}
	return id, nil
}

// ClientTenants は、所属の記録のある RADIUSクライアントすべてについて、ID → テナントを返す
// （RADIUSクライアントは provisioner の全件を所属で絞るので、記録も全件を読む。設計概要 §7.1）。
func (d *DB) ClientTenants(ctx context.Context) (map[int64]int64, error) {
	rows, err := d.Pool.Query(ctx, "SELECT client_id, tenant_id FROM client_tenants")
	if err != nil {
		return nil, fmt.Errorf("client tenants: %w", err)
	}
	out := map[int64]int64{}
	var clientID, tenantID int64
	if _, err := pgx.ForEachRow(rows, []any{&clientID, &tenantID}, func() error {
		out[clientID] = tenantID
		return nil
	}); err != nil {
		return nil, fmt.Errorf("client tenants: %w", err)
	}
	return out, nil
}

// AssignClient は RADIUSクライアントの所属を tenantID にする。tenantID が 0 なら記録を消す（テナントなし）。
func (d *DB) AssignClient(ctx context.Context, clientID, tenantID int64, by string) error {
	var err error
	if tenantID == 0 {
		_, err = d.Pool.Exec(ctx, "DELETE FROM client_tenants WHERE client_id = $1", clientID)
	} else {
		_, err = d.Pool.Exec(ctx, `
			INSERT INTO client_tenants (client_id, tenant_id, assigned_by) VALUES ($1, $2, $3)
			ON CONFLICT (client_id) DO UPDATE SET tenant_id = EXCLUDED.tenant_id, assigned_at = now(), assigned_by = EXCLUDED.assigned_by`,
			clientID, tenantID, by)
	}
	if err != nil {
		return fmt.Errorf("assign client: %w", err)
	}
	return nil
}

// Assignment は所属の記録の 1 件（下流にない所属の記録の点検に使う。画面仕様 §7.2）。
type Assignment struct {
	// IMSI は加入者の記録、ClientID は RADIUSクライアントの記録（どちらか一方）。
	IMSI       string
	ClientID   int64
	TenantID   int64
	AssignedAt time.Time
	AssignedBy string
}

// SubscriberAssignments は、加入者の所属の記録を IMSI の順に、after より後ろから limit 件まで返す。
func (d *DB) SubscriberAssignments(ctx context.Context, after string, limit int) ([]Assignment, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT imsi, tenant_id, assigned_at, assigned_by FROM subscriber_tenants
		WHERE imsi > $1 ORDER BY imsi LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("subscriber assignments: %w", err)
	}
	var out []Assignment
	var a Assignment
	if _, err := pgx.ForEachRow(rows, []any{&a.IMSI, &a.TenantID, &a.AssignedAt, &a.AssignedBy}, func() error {
		out = append(out, a)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("subscriber assignments: %w", err)
	}
	return out, nil
}

// ClientAssignments は、RADIUSクライアントの所属の記録をすべて ID の順に返す。
func (d *DB) ClientAssignments(ctx context.Context) ([]Assignment, error) {
	rows, err := d.Pool.Query(ctx, "SELECT client_id, tenant_id, assigned_at, assigned_by FROM client_tenants ORDER BY client_id")
	if err != nil {
		return nil, fmt.Errorf("client assignments: %w", err)
	}
	var out []Assignment
	var a Assignment
	if _, err := pgx.ForEachRow(rows, []any{&a.ClientID, &a.TenantID, &a.AssignedAt, &a.AssignedBy}, func() error {
		out = append(out, a)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("client assignments: %w", err)
	}
	return out, nil
}
