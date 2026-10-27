package db

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
)

// AuditEntry は本BFF の監査ログの 1 件（設計概要 §13）。Detail に秘密の値（Ki / OPc、共有シークレット）を入れてはならない。
type AuditEntry struct {
	ActorEmail string
	ActorRole  authz.Role
	// TenantID は対象のテナント（0 はテナントなし。DB には NULL で入れる）。
	TenantID int64
	Action   string
	Target   string
	Detail   map[string]any
	TraceID  string
	Remote   string
}

// InsertAudit は監査ログを 1 件書き込む。
func (d *DB) InsertAudit(ctx context.Context, e AuditEntry) error {
	detail := []byte("{}")
	if len(e.Detail) > 0 {
		b, err := json.Marshal(e.Detail, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("insert audit: encode detail: %w", err)
		}
		detail = b
	}
	var tenant *int64
	if e.TenantID != 0 {
		tenant = &e.TenantID
	}
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO audit_log (actor_email, actor_role, tenant_id, action, target, detail, trace_id, remote)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8)`,
		e.ActorEmail, string(e.ActorRole), tenant, e.Action, e.Target, string(detail), e.TraceID, e.Remote)
	if err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}

// AuditRecord は監査ログの 1 件（読み出し）。
type AuditRecord struct {
	ID         int64
	Time       time.Time
	ActorEmail string
	ActorRole  authz.Role
	// TenantID は対象のテナント（0 はテナントなし）。
	TenantID int64
	Action   string
	Target   string
	Detail   map[string]any
	TraceID  string
	Remote   string
}

// AuditQuery は監査ログの読み出しの条件（画面仕様 §11）。
type AuditQuery struct {
	// AllTenants ならすべて、NoTenant ならテナントなし、どちらでもなければ TenantID のもの。
	AllTenants, NoTenant bool
	TenantID             int64
	// Action が空でなければ、その操作だけ。
	Action string
	// Before が 0 でなければ、その ID より古いものだけ（続きの読み込み）。
	Before int64
	Limit  int
}

// ListAudit は監査ログを新しい順に返す。
func (d *DB) ListAudit(ctx context.Context, q AuditQuery) ([]AuditRecord, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	switch {
	case q.NoTenant:
		where = append(where, "tenant_id IS NULL")
	case !q.AllTenants:
		add("tenant_id = $%d", q.TenantID)
	}
	if q.Action != "" {
		add("action = $%d", q.Action)
	}
	if q.Before != 0 {
		add("id < $%d", q.Before)
	}
	sql := "SELECT id, time, actor_email, actor_role, tenant_id, action, target, detail, trace_id, remote FROM audit_log"
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, q.Limit)
	sql += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := d.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	var out []AuditRecord
	for rows.Next() {
		var r AuditRecord
		var tenant *int64
		var detail []byte
		if err := rows.Scan(&r.ID, &r.Time, &r.ActorEmail, &r.ActorRole, &tenant, &r.Action, &r.Target, &detail, &r.TraceID, &r.Remote); err != nil {
			return nil, fmt.Errorf("list audit: %w", err)
		}
		if tenant != nil {
			r.TenantID = *tenant
		}
		if err := json.Unmarshal(detail, &r.Detail); err != nil {
			return nil, fmt.Errorf("list audit: decode detail of %d: %w", r.ID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	return out, nil
}
