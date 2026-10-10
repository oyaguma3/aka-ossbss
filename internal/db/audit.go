package db

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
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
