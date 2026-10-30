package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 台帳と下流の突き合わせの記録（設計概要 §9。段階 3-3 の Y7）。判定と修正は internal/reconcile。

// 突き合わせの実行のきっかけ。
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerApproved = "approved"
)

// 突き合わせの実行の状態。
const (
	RunRunning     = "running"
	RunCompleted   = "completed"
	RunHeld        = "held"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"
)

// ずれの種類。
const (
	FindingLedgerOnly     = "ledger_only"
	FindingStateDiff      = "state_diff"
	FindingSourceDiff     = "source_diff"
	FindingSideAbsent     = "side_absent"
	FindingDownstreamOnly = "downstream_only"
	FindingDeletedPresent = "deleted_present"
	FindingRuleTenant     = "rule_tenant"
)

// ずれの状態。
const (
	FindingFixed    = "fixed"
	FindingHeld     = "held"
	FindingPending  = "pending"
	FindingInfo     = "info"
	FindingImported = "imported"
	FindingDeleted  = "deleted"
	FindingGone     = "gone"
)

// ReconcileRun は突き合わせの 1 回の実行。
type ReconcileRun struct {
	ID            int64
	Trigger       string
	RequestedBy   string
	ApproveReason string
	State         string
	// LedgerRows・VecgenRows・RadiusRows は読んだ行の数。
	LedgerRows, VecgenRows, RadiusRows int
	// FixableCount は自動で直す対象の数、FixedCount は直した数、FindingsTotal は見つけたずれの総数。
	FixableCount, FixedCount, FindingsTotal int
	Error                                   string
	StartedAt, FinishedAt                   time.Time
}

// Finding は見つけたずれ。
type Finding struct {
	ID    int64
	RunID int64
	IMSI  string
	Kind  string
	Side  Side
	// LedgerVal と DownstreamVal は台帳と下流の値（画面に出す）。
	LedgerVal, DownstreamVal string
	// TenantID は所属のテナント（台帳にないものは取り込みの初期値の候補。分からなければ 0）。
	TenantID   int64
	Status     string
	ResolvedBy string
	ResolvedAt time.Time
}

const reconcileRunColumns = `id, trigger, requested_by, approve_reason, state, ledger_rows, vecgen_rows, radius_rows,
	fixable_count, fixed_count, findings_total, error, started_at, finished_at`

func scanReconcileRun(row pgx.Row) (ReconcileRun, error) {
	var r ReconcileRun
	var finished *time.Time
	if err := row.Scan(&r.ID, &r.Trigger, &r.RequestedBy, &r.ApproveReason, &r.State, &r.LedgerRows, &r.VecgenRows, &r.RadiusRows,
		&r.FixableCount, &r.FixedCount, &r.FindingsTotal, &r.Error, &r.StartedAt, &finished); err != nil {
		return ReconcileRun{}, err
	}
	if finished != nil {
		r.FinishedAt = *finished
	}
	return r, nil
}

// CreateReconcileRun は実行を始める。
func (d *DB) CreateReconcileRun(ctx context.Context, trigger, by, reason string) (ReconcileRun, error) {
	r, err := scanReconcileRun(d.Pool.QueryRow(ctx, `INSERT INTO reconcile_runs (trigger, requested_by, approve_reason)
		VALUES ($1, $2, $3) RETURNING `+reconcileRunColumns, trigger, by, reason))
	if err != nil {
		return ReconcileRun{}, fmt.Errorf("create reconcile run: %w", err)
	}
	return r, nil
}

// UpdateReconcileProgress は読んだ行の数を書く。
func (d *DB) UpdateReconcileProgress(ctx context.Context, id int64, ledger, vecgen, radius int) error {
	_, err := d.Pool.Exec(ctx, "UPDATE reconcile_runs SET ledger_rows = $2, vecgen_rows = $3, radius_rows = $4 WHERE id = $1", id, ledger, vecgen, radius)
	if err != nil {
		return fmt.Errorf("update reconcile progress: %w", err)
	}
	return nil
}

// FinishReconcileRun は実行を終える。
func (d *DB) FinishReconcileRun(ctx context.Context, r ReconcileRun) (ReconcileRun, error) {
	out, err := scanReconcileRun(d.Pool.QueryRow(ctx, `UPDATE reconcile_runs SET state = $2, ledger_rows = $3, vecgen_rows = $4, radius_rows = $5,
			fixable_count = $6, fixed_count = $7, findings_total = $8, error = $9, finished_at = now()
		WHERE id = $1 RETURNING `+reconcileRunColumns,
		r.ID, r.State, r.LedgerRows, r.VecgenRows, r.RadiusRows, r.FixableCount, r.FixedCount, r.FindingsTotal, r.Error))
	if err != nil {
		return ReconcileRun{}, fmt.Errorf("finish reconcile run: %w", err)
	}
	return out, nil
}

// InterruptRunningReconciles は、実行中のまま残っている実行を中断にする（起動のとき）。
func (d *DB) InterruptRunningReconciles(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, "UPDATE reconcile_runs SET state = 'interrupted', finished_at = now() WHERE state = 'running'")
	if err != nil {
		return fmt.Errorf("interrupt reconcile runs: %w", err)
	}
	return nil
}

// ReconcileRun は実行を返す。なければ ErrNotFound。
func (d *DB) ReconcileRun(ctx context.Context, id int64) (ReconcileRun, error) {
	r, err := scanReconcileRun(d.Pool.QueryRow(ctx, "SELECT "+reconcileRunColumns+" FROM reconcile_runs WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ReconcileRun{}, ErrNotFound
	}
	if err != nil {
		return ReconcileRun{}, fmt.Errorf("reconcile run: %w", err)
	}
	return r, nil
}

// ReconcileRuns は実行を新しい順に limit 件まで返す。
func (d *DB) ReconcileRuns(ctx context.Context, limit int) ([]ReconcileRun, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+reconcileRunColumns+" FROM reconcile_runs ORDER BY id DESC LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("reconcile runs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReconcileRun, error) { return scanReconcileRun(row) })
	if err != nil {
		return nil, fmt.Errorf("reconcile runs: %w", err)
	}
	return out, nil
}

// LatestFinishedReconcileRun は、ずれの一覧がそろった最新の実行（完了か保留）を返す（なければ false）。対応できるのはこの実行のずれだけ（Y7）。
func (d *DB) LatestFinishedReconcileRun(ctx context.Context) (ReconcileRun, bool, error) {
	r, err := scanReconcileRun(d.Pool.QueryRow(ctx, "SELECT "+reconcileRunColumns+" FROM reconcile_runs WHERE state IN ('completed', 'held') ORDER BY id DESC LIMIT 1"))
	if errors.Is(err, pgx.ErrNoRows) {
		return ReconcileRun{}, false, nil
	}
	if err != nil {
		return ReconcileRun{}, false, fmt.Errorf("latest reconcile run: %w", err)
	}
	return r, true, nil
}

// AddFindings はずれを書く。
func (d *DB) AddFindings(ctx context.Context, runID int64, fs []Finding) error {
	if len(fs) == 0 {
		return nil
	}
	_, err := d.Pool.CopyFrom(ctx, pgx.Identifier{"reconcile_findings"},
		[]string{"run_id", "imsi", "kind", "side", "ledger_val", "downstream_val", "tenant_id", "status"},
		pgx.CopyFromSlice(len(fs), func(i int) ([]any, error) {
			f := fs[i]
			return []any{runID, f.IMSI, f.Kind, string(f.Side), f.LedgerVal, f.DownstreamVal, f.TenantID, f.Status}, nil
		}))
	if err != nil {
		return fmt.Errorf("add findings: %w", err)
	}
	return nil
}

const findingColumns = "id, run_id, imsi, kind, side, ledger_val, downstream_val, tenant_id, status, resolved_by, resolved_at"

func scanFinding(row pgx.Row) (Finding, error) {
	var f Finding
	var side string
	var resolved *time.Time
	if err := row.Scan(&f.ID, &f.RunID, &f.IMSI, &f.Kind, &side, &f.LedgerVal, &f.DownstreamVal, &f.TenantID, &f.Status, &f.ResolvedBy, &resolved); err != nil {
		return Finding{}, err
	}
	f.Side = Side(side)
	if resolved != nil {
		f.ResolvedAt = *resolved
	}
	return f, nil
}

// FindingQuery はずれの一覧の条件。
type FindingQuery struct {
	RunID int64
	// Kind と Status は空なら絞らない。
	Kind, Status string
	// AfterID より後ろから Limit 件まで返す。
	AfterID int64
	Limit   int
}

// where は条件の WHERE 句と引数を作る。
func (q FindingQuery) where() (string, []any) {
	conds := []string{"run_id = $1"}
	args := []any{q.RunID}
	if q.Kind != "" {
		args = append(args, q.Kind)
		conds = append(conds, "kind = $"+strconv.Itoa(len(args)))
	}
	if q.Status != "" {
		args = append(args, q.Status)
		conds = append(conds, "status = $"+strconv.Itoa(len(args)))
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ReconcileFindings はずれを ID の順に返す。
func (d *DB) ReconcileFindings(ctx context.Context, q FindingQuery) ([]Finding, error) {
	where, args := q.where()
	args = append(args, q.AfterID, q.Limit)
	rows, err := d.Pool.Query(ctx, fmt.Sprintf("SELECT %s FROM reconcile_findings%s AND id > $%d ORDER BY id LIMIT $%d",
		findingColumns, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("reconcile findings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Finding, error) { return scanFinding(row) })
	if err != nil {
		return nil, fmt.Errorf("reconcile findings: %w", err)
	}
	return out, nil
}

// FindingCount は種類と状態ごとの、行として残したずれの数。
type FindingCount struct {
	Kind, Status string
	Count        int
}

// CountFindings は種類と状態ごとの数を返す。
func (d *DB) CountFindings(ctx context.Context, runID int64) ([]FindingCount, error) {
	rows, err := d.Pool.Query(ctx, "SELECT kind, status, count(*) FROM reconcile_findings WHERE run_id = $1 GROUP BY 1, 2 ORDER BY 1, 2", runID)
	if err != nil {
		return nil, fmt.Errorf("count findings: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[FindingCount])
	if err != nil {
		return nil, fmt.Errorf("count findings: %w", err)
	}
	return out, nil
}

// ReconcileFinding はずれを返す。なければ ErrNotFound。
func (d *DB) ReconcileFinding(ctx context.Context, id int64) (Finding, error) {
	f, err := scanFinding(d.Pool.QueryRow(ctx, "SELECT "+findingColumns+" FROM reconcile_findings WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Finding{}, ErrNotFound
	}
	if err != nil {
		return Finding{}, fmt.Errorf("reconcile finding: %w", err)
	}
	return f, nil
}

// ResolveFindings は、実行の IMSI の未対応のずれ（side が空でなければその側だけ）に対応の結果を書き、書いた数を返す。
func (d *DB) ResolveFindings(ctx context.Context, runID int64, imsi string, side Side, status, by string) (int64, error) {
	tag, err := d.Pool.Exec(ctx, `UPDATE reconcile_findings SET status = $4, resolved_by = $5, resolved_at = now()
		WHERE run_id = $1 AND imsi = $2 AND ($3 = '' OR side = $3) AND status = 'pending'`, runID, imsi, string(side), status, by)
	if err != nil {
		return 0, fmt.Errorf("resolve findings: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReconcileEntries は台帳の行を、削除済みを含めて IMSI の順に、after より後ろから limit 件まで返す（突き合わせの読み取り）。
func (d *DB) ReconcileEntries(ctx context.Context, after string, limit int) ([]Entry, error) {
	rows, err := d.Pool.Query(ctx, entrySelect+" WHERE imsi > $1 ORDER BY imsi LIMIT $2", after, limit)
	if err != nil {
		return nil, fmt.Errorf("reconcile entries: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) { return scanEntry(row) })
	if err != nil {
		return nil, fmt.Errorf("reconcile entries: %w", err)
	}
	return out, nil
}

// CheckNeededIMSIs は結果を確かめる印の立った IMSI を、削除済みを含めて IMSI の順に limit 件まで返す（5 分ごとの確かめ。Y10）。
func (d *DB) CheckNeededIMSIs(ctx context.Context, limit int) ([]string, error) {
	rows, err := d.Pool.Query(ctx, "SELECT imsi FROM subscribers WHERE check_needed ORDER BY imsi LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("check needed imsis: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("check needed imsis: %w", err)
	}
	return out, nil
}
