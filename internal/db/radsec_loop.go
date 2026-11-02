package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 中央の整合のループ（internal/radsec。段階 4-4）が使う DB の操作。望む状態は画面が書き、ここではエッジの状態の写しとループの途中の状態を書く。

// 配置の apply_result のうち、エッジの反映の結果（ok・failed・rolled_back・interrupted）でないもの。
const (
	// ApplyResultSyncError はエッジや radsec-ca が断って、宣言か証明書を送れなかったこと（次の周回でやり直す）。
	ApplyResultSyncError = "sync_error"
	// ApplyResultStopped はエッジの画面でテナントを止めていること（反映を飛ばす。OB2）。
	ApplyResultStopped = "stopped"
)

// 証明書の種類。
const (
	CertKindServer = "server"
	CertKindProbe  = "probe"
)

// SetPlacementApply はエッジの状態の写しを書く。
func (d *DB) SetPlacementApply(ctx context.Context, id, applied, failed int64, result, errText string) error {
	if _, err := d.Pool.Exec(ctx, `UPDATE placements SET applied_revision = $2, failed_revision = $3, apply_result = $4, apply_error = $5, checked_at = now() WHERE id = $1`,
		id, applied, failed, result, errText); err != nil {
		return fmt.Errorf("set placement apply: %w", err)
	}
	return nil
}

// TouchPlacements はエッジの配置を確かめた時刻を書く（状態が変わらないときの行ごとの書き込みを避ける）。
func (d *DB) TouchPlacements(ctx context.Context, edge string) error {
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET checked_at = now() WHERE edge_id = $1 AND removed_at IS NULL", edge); err != nil {
		return fmt.Errorf("touch placements: %w", err)
	}
	return nil
}

// MarkPlacementRemoved はエッジから消し終えたことを記録する（容量はここまで数える）。
func (d *DB) MarkPlacementRemoved(ctx context.Context, id int64) error {
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET removed_at = now(), checked_at = now() WHERE id = $1", id); err != nil {
		return fmt.Errorf("mark placement removed: %w", err)
	}
	return nil
}

// certColumns は証明書の種類の列の名前を返す。
func certColumns(kind string) (serial, notAfter string) {
	if kind == CertKindProbe {
		return "probe_serial", "probe_not_after"
	}
	return "server_serial", "server_not_after"
}

// RecordPlacementCert はエッジに置いてある証明書の控えを書く（控えがないときだけ。反映は求めない）。
func (d *DB) RecordPlacementCert(ctx context.Context, id int64, kind, serial string, notAfter time.Time) error {
	s, n := certColumns(kind)
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET "+s+" = $2, "+n+" = $3 WHERE id = $1", id, serial, notAfter); err != nil {
		return fmt.Errorf("record placement cert: %w", err)
	}
	return nil
}

// PlacementCertPlaced は新しい証明書をエッジに置いたことを記録する。世代を進め、反映が要る印を立て、前の証明書を失効の待ちに移す
// （同じ証明書なら移さない。同じ要求の再送で同じ証明書が返るため）。
func (d *DB) PlacementCertPlaced(ctx context.Context, id int64, kind, serial string, notAfter time.Time) error {
	s, n := certColumns(kind)
	if _, err := d.Pool.Exec(ctx, `UPDATE placements SET
		superseded_serials = CASE WHEN `+s+` <> '' AND `+s+` <> $2 AND NOT `+s+` = ANY(superseded_serials) THEN array_append(superseded_serials, `+s+`) ELSE superseded_serials END,
		`+s+` = $2, `+n+` = $3, cert_generation = cert_generation + 1, cert_apply_needed = true WHERE id = $1`, id, serial, notAfter); err != nil {
		return fmt.Errorf("placement cert placed: %w", err)
	}
	return nil
}

// BumpCertGeneration は発行の世代を進める（4xx の後は新しいキーでやり直す。4xx も覚えられるため）。
func (d *DB) BumpCertGeneration(ctx context.Context, id int64) error {
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET cert_generation = cert_generation + 1 WHERE id = $1", id); err != nil {
		return fmt.Errorf("bump cert generation: %w", err)
	}
	return nil
}

// ClearRenew は更新の印を消す。
func (d *DB) ClearRenew(ctx context.Context, id int64) error {
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET renew_requested = false WHERE id = $1", id); err != nil {
		return fmt.Errorf("clear renew: %w", err)
	}
	return nil
}

// FinishCertApply は証明書の反映が済んだことを記録する。remaining はまだ失効させられなかった古い証明書。
func (d *DB) FinishCertApply(ctx context.Context, id int64, remaining []string) error {
	if remaining == nil {
		remaining = []string{} // 列は NOT NULL
	}
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET cert_apply_needed = false, superseded_serials = $2 WHERE id = $1", id, remaining); err != nil {
		return fmt.Errorf("finish cert apply: %w", err)
	}
	return nil
}

// ClearRetry はやり直しの印と、失敗した版の記録を消す（反映を始めたとき）。
func (d *DB) ClearRetry(ctx context.Context, id int64) error {
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET retry_requested = false, failed_revision = 0 WHERE id = $1", id); err != nil {
		return fmt.Errorf("clear retry: %w", err)
	}
	return nil
}

// RequestRetry は失敗した反映のやり直しを求める（B・C・D の「やり直す」。OB2）。配置がなければ ErrNotFound。
func (d *DB) RequestRetry(ctx context.Context, tenant, id int64) error {
	tag, err := d.Pool.Exec(ctx, "UPDATE placements SET retry_requested = true WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL", id, tenant)
	if err != nil {
		return fmt.Errorf("request retry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearCRLDirty は CRL の取り直しの印を消す。失効の操作は版も上げるので、読んだ版のままのときだけ消す（取り直しの間の新しい失効を取りこぼさない）。
func (d *DB) ClearCRLDirty(ctx context.Context, tenant, revision int64) error {
	if _, err := d.Pool.Exec(ctx, "UPDATE tenant_radsec SET crl_dirty = false WHERE tenant_id = $1 AND revision = $2", tenant, revision); err != nil {
		return fmt.Errorf("clear crl dirty: %w", err)
	}
	return nil
}

// RemovingBroughtCAs は外している途中の持ち込みの CA を返す。
func (d *DB) RemovingBroughtCAs(ctx context.Context) ([]BroughtCA, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+broughtColumns+" FROM brought_cas WHERE removing_at IS NOT NULL AND deleted_at IS NULL ORDER BY ca_id")
	if err != nil {
		return nil, fmt.Errorf("removing brought cas: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BroughtCA, error) { return scanBrought(row) })
	if err != nil {
		return nil, fmt.Errorf("removing brought cas: %w", err)
	}
	return out, nil
}

// FinishRemoveBrought は持ち込みの CA を外し終えたことを記録する（課金の数量はここまで数える）。
func (d *DB) FinishRemoveBrought(ctx context.Context, caID string) error {
	if _, err := d.Pool.Exec(ctx, "UPDATE brought_cas SET deleted_at = now() WHERE ca_id = $1 AND deleted_at IS NULL", caID); err != nil {
		return fmt.Errorf("finish remove brought: %w", err)
	}
	return nil
}

// RadsecCADisablePending は削除したテナントのうち、CA をまだ止めていないものを返す（OB12）。
func (d *DB) RadsecCADisablePending(ctx context.Context) ([]TenantRadsec, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+radsecColumns+" FROM tenant_radsec WHERE released_at IS NOT NULL AND ca_disabled_at IS NULL ORDER BY tenant_id")
	if err != nil {
		return nil, fmt.Errorf("radsec ca disable pending: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TenantRadsec, error) { return scanRadsec(row) })
	if err != nil {
		return nil, fmt.Errorf("radsec ca disable pending: %w", err)
	}
	return out, nil
}

// MarkRenewals は証明書の期限が before より前の配置に更新の印を立て、立てた数を返す（OB4）。
func (d *DB) MarkRenewals(ctx context.Context, before time.Time) (int64, error) {
	tag, err := d.Pool.Exec(ctx, `UPDATE placements SET renew_requested = true
		WHERE deleted_at IS NULL AND NOT renew_requested AND (server_not_after < $1 OR probe_not_after < $1)`, before)
	if err != nil {
		return 0, fmt.Errorf("mark renewals: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ExpiringAPCert は期限の近い AP・グループ。
type ExpiringAPCert struct {
	TenantID   int64
	TenantName string
	AP         string
	Kind       string
	Serial     string
	NotAfter   time.Time
}

// ExpiringAPCerts は、有効な証明書のうち一番新しいものの期限が before より前の AP・グループを、期限の近い順に返す（更新は人が行う）。
func (d *DB) ExpiringAPCerts(ctx context.Context, before time.Time) ([]ExpiringAPCert, error) {
	rows, err := d.Pool.Query(ctx, `SELECT a.tenant_id, t.name, a.name, a.kind, c.serial, c.not_after
		FROM aps a JOIN tenants t ON t.id = a.tenant_id
		JOIN LATERAL (SELECT serial, not_after FROM ap_certs WHERE ap_id = a.id AND revoked_at IS NULL ORDER BY not_after DESC LIMIT 1) c ON true
		WHERE a.deleted_at IS NULL AND c.not_after < $1 ORDER BY c.not_after, a.id`, before)
	if err != nil {
		return nil, fmt.Errorf("expiring ap certs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ExpiringAPCert, error) {
		var c ExpiringAPCert
		err := row.Scan(&c.TenantID, &c.TenantName, &c.AP, &c.Kind, &c.Serial, &c.NotAfter)
		return c, err
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("expiring ap certs: %w", err)
	}
	return out, nil
}
