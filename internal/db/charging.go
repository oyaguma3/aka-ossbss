package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 課金レコードの読み書き（課金レコードの要件と設計。段階 3-4 の Z2〜Z13）。計算は internal/charging が行う。

// JST は課金レコードの月と日の区切りの日本時間（夏時間はないので固定の +09:00 にする）。
var JST = time.FixedZone("JST", 9*60*60)

// MonthOf は t を含む日本時間の月の 1 日 0 時を返す。
func MonthOf(t time.Time) time.Time {
	n := t.In(JST)
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, JST)
}

// asDate は DB の date の値（UTC の 0 時で読める）を、同じ日付の日本時間の 0 時にする。
func asDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, JST)
}

// TenantExternalReason は、外部の顧客番号の変更の事実（tenant_updated）の理由（名前の変更と見分ける。Z15）。
const TenantExternalReason = "外部の顧客番号の変更"

// ---- 利用の集計 ----

// Usage は利用の数量（課金レコード §5.2）。
type Usage struct {
	AuthOK, Sessions, Seconds, InOctets, OutOctets int64
}

// UsageRow は IMSI とテナント（集計の刻印。なければ 0）ごとの利用の集計。
type UsageRow struct {
	IMSI     string
	TenantID int64
	Usage
}

// EnsureUsagePartition は date を含む月の usage_daily のパーティションを作る（あれば何もしない）。
func (d *DB) EnsureUsagePartition(ctx context.Context, date time.Time) error {
	m := MonthOf(date)
	_, err := d.Pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF usage_daily FOR VALUES FROM ('%s') TO ('%s')`,
		usagePartition(m), m.Format(time.DateOnly), m.AddDate(0, 1, 0).Format(time.DateOnly)))
	if err != nil {
		return fmt.Errorf("usage partition %s: %w", m.Format("200601"), err)
	}
	return nil
}

// usagePartition はその月のパーティションの名前。
func usagePartition(month time.Time) string { return "usage_daily_" + month.Format("200601") }

// UpsertUsage はその日の利用の集計を書き、書いた行の数を返す。値が変わらない行は書かない（Z6）。
func (d *DB) UpsertUsage(ctx context.Context, date time.Time, rows []UsageRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	imsi, tenant := make([]string, len(rows)), make([]int64, len(rows))
	auth, sessions, seconds, in, out := make([]int64, len(rows)), make([]int64, len(rows)), make([]int64, len(rows)), make([]int64, len(rows)), make([]int64, len(rows))
	for i, r := range rows {
		imsi[i], tenant[i], auth[i], sessions[i], seconds[i], in[i], out[i] = r.IMSI, r.TenantID, r.AuthOK, r.Sessions, r.Seconds, r.InOctets, r.OutOctets
	}
	tag, err := d.Pool.Exec(ctx, `
		INSERT INTO usage_daily (date, imsi, tenant_id, auth_ok, sessions, seconds, in_octets, out_octets)
		SELECT $1::date, * FROM unnest($2::text[], $3::bigint[], $4::bigint[], $5::bigint[], $6::bigint[], $7::bigint[], $8::bigint[])
		ON CONFLICT (imsi, tenant_id, date) DO UPDATE SET auth_ok = EXCLUDED.auth_ok, sessions = EXCLUDED.sessions, seconds = EXCLUDED.seconds,
			in_octets = EXCLUDED.in_octets, out_octets = EXCLUDED.out_octets
		WHERE (usage_daily.auth_ok, usage_daily.sessions, usage_daily.seconds, usage_daily.in_octets, usage_daily.out_octets)
			IS DISTINCT FROM (EXCLUDED.auth_ok, EXCLUDED.sessions, EXCLUDED.seconds, EXCLUDED.in_octets, EXCLUDED.out_octets)`,
		date, imsi, tenant, auth, sessions, seconds, in, out)
	if err != nil {
		return 0, fmt.Errorf("upsert usage: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RecordUsageImport は日ごとの取り込みの成否を書く（errText が空なら読み切った）。一度読み切った日は、後の読み直しに失敗しても
// 読み切った日のままにする（値はそのときのものが残っている）。
func (d *DB) RecordUsageImport(ctx context.Context, date time.Time, errText string, at time.Time) error {
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO usage_import_days (date, attempted_at, imported_at, last_error)
		VALUES ($1, $2, CASE WHEN $3 = '' THEN $2::timestamptz END, $3)
		ON CONFLICT (date) DO UPDATE SET attempted_at = EXCLUDED.attempted_at, last_error = EXCLUDED.last_error,
			imported_at = coalesce(EXCLUDED.imported_at, usage_import_days.imported_at)`, date, at, errText)
	if err != nil {
		return fmt.Errorf("record usage import: %w", err)
	}
	return nil
}

// UsageImportedDays は [start, end) のうち読み切った日を返す。
func (d *DB) UsageImportedDays(ctx context.Context, start, end time.Time) ([]time.Time, error) {
	rows, err := d.Pool.Query(ctx, "SELECT date FROM usage_import_days WHERE date >= $1 AND date < $2 AND imported_at IS NOT NULL ORDER BY date", start, end)
	if err != nil {
		return nil, fmt.Errorf("usage imported days: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (time.Time, error) {
		var t time.Time
		err := row.Scan(&t)
		return asDate(t), err
	})
	if err != nil {
		return nil, fmt.Errorf("usage imported days: %w", err)
	}
	return out, nil
}

// UsageImportFailure は読み切っていない日。
type UsageImportFailure struct {
	Date        time.Time
	AttemptedAt time.Time
	Error       string
}

// UsageImportFailures は読み切っていない日を新しい順に返す（画面に出す）。
func (d *DB) UsageImportFailures(ctx context.Context, limit int) ([]UsageImportFailure, error) {
	rows, err := d.Pool.Query(ctx, `SELECT date, attempted_at, last_error FROM usage_import_days WHERE imported_at IS NULL ORDER BY date DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("usage import failures: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageImportFailure, error) {
		var f UsageImportFailure
		err := row.Scan(&f.Date, &f.AttemptedAt, &f.Error)
		f.Date = asDate(f.Date)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("usage import failures: %w", err)
	}
	return out, nil
}

// MonthUsage は [start, end) の利用の集計を、IMSI とテナントごとに合計して、その順に after より後ろから limit 件返す。
func (d *DB) MonthUsage(ctx context.Context, start, end time.Time, afterIMSI string, afterTenant int64, limit int) ([]UsageRow, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT imsi, tenant_id, sum(auth_ok)::bigint, sum(sessions)::bigint, sum(seconds)::bigint, sum(in_octets)::bigint, sum(out_octets)::bigint
		FROM usage_daily WHERE date >= $1 AND date < $2 AND (imsi, tenant_id) > ($3, $4)
		GROUP BY imsi, tenant_id ORDER BY imsi, tenant_id LIMIT $5`, start, end, afterIMSI, afterTenant, limit)
	if err != nil {
		return nil, fmt.Errorf("month usage: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (UsageRow, error) {
		var r UsageRow
		err := row.Scan(&r.IMSI, &r.TenantID, &r.AuthOK, &r.Sessions, &r.Seconds, &r.InOctets, &r.OutOctets)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("month usage: %w", err)
	}
	return out, nil
}

// ---- 状態の事実の読み出し ----

const chargingEventColumns = `id, time, event, coalesce(side, ''), tenant_id, coalesce(imsi, ''), from_val, to_val, operator, job_id, trace_id, reason`

func scanChargingEvent(row pgx.CollectableRow) (ChargingEvent, error) {
	var ev ChargingEvent
	err := row.Scan(&ev.ID, &ev.Time, &ev.Event, &ev.Side, &ev.TenantID, &ev.IMSI, &ev.From, &ev.To, &ev.Operator, &ev.JobID, &ev.TraceID, &ev.Reason)
	return ev, err
}

// MonthLineEvents は [start, end) の回線の事実を (IMSI, 番号) の順に、after より後ろから limit 件返す。
func (d *DB) MonthLineEvents(ctx context.Context, start, end time.Time, afterIMSI string, afterID int64, limit int) ([]ChargingEvent, error) {
	rows, err := d.Pool.Query(ctx, `SELECT `+chargingEventColumns+` FROM charging_events
		WHERE imsi IS NOT NULL AND time >= $1 AND time < $2 AND (imsi, id) > ($3, $4) ORDER BY imsi, id LIMIT $5`, start, end, afterIMSI, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("month line events: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanChargingEvent)
	if err != nil {
		return nil, fmt.Errorf("month line events: %w", err)
	}
	return out, nil
}

// MonthTenantEvents は [start, end) のテナントの事実を番号の順に返す。
func (d *DB) MonthTenantEvents(ctx context.Context, start, end time.Time) ([]ChargingEvent, error) {
	rows, err := d.Pool.Query(ctx, `SELECT `+chargingEventColumns+` FROM charging_events
		WHERE imsi IS NULL AND time >= $1 AND time < $2 ORDER BY id`, start, end)
	if err != nil {
		return nil, fmt.Errorf("month tenant events: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanChargingEvent)
	if err != nil {
		return nil, fmt.Errorf("month tenant events: %w", err)
	}
	return out, nil
}

// OldestChargingEvent は最も古い事実の時刻を返す（なければ false）。
func (d *DB) OldestChargingEvent(ctx context.Context) (time.Time, bool, error) {
	var t *time.Time
	if err := d.Pool.QueryRow(ctx, "SELECT min(time) FROM charging_events").Scan(&t); err != nil {
		return time.Time{}, false, fmt.Errorf("oldest charging event: %w", err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}

// ---- 締めた月 ----

// 締めた月の状態。
const (
	PeriodComputed = "computed"
	PeriodFiled    = "filed"
)

// ChargingPeriod は締めた月。
type ChargingPeriod struct {
	// Period は対象月の 1 日（日本時間の 0 時）。
	Period                                       time.Time
	Sequence                                     int
	State                                        string
	EventCount, LineCount, TenantCount, AdjCount int
	MissingDays                                  int
	ClosedAt                                     time.Time
	ClosedBy                                     string
}

const chargingPeriodColumns = `period, sequence, state, event_count, line_count, tenant_count, adj_count, missing_days, closed_at, closed_by`

func scanChargingPeriod(row pgx.Row) (ChargingPeriod, error) {
	var p ChargingPeriod
	err := row.Scan(&p.Period, &p.Sequence, &p.State, &p.EventCount, &p.LineCount, &p.TenantCount, &p.AdjCount, &p.MissingDays, &p.ClosedAt, &p.ClosedBy)
	p.Period = asDate(p.Period)
	return p, err
}

// ChargingPeriods は締めた月を新しい順に返す。
func (d *DB) ChargingPeriods(ctx context.Context, limit int) ([]ChargingPeriod, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+chargingPeriodColumns+" FROM charging_periods ORDER BY period DESC LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("charging periods: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargingPeriod, error) { return scanChargingPeriod(row) })
	if err != nil {
		return nil, fmt.Errorf("charging periods: %w", err)
	}
	return out, nil
}

// ChargingPeriodOf は対象月の締めた月を返す。なければ ErrNotFound。
func (d *DB) ChargingPeriodOf(ctx context.Context, month time.Time) (ChargingPeriod, error) {
	p, err := scanChargingPeriod(d.Pool.QueryRow(ctx, "SELECT "+chargingPeriodColumns+" FROM charging_periods WHERE period = $1", month))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChargingPeriod{}, ErrNotFound
	}
	if err != nil {
		return ChargingPeriod{}, fmt.Errorf("charging period: %w", err)
	}
	return p, nil
}

// LatestChargingPeriod は最も新しい締めた月を返す（なければ false）。
func (d *DB) LatestChargingPeriod(ctx context.Context) (ChargingPeriod, bool, error) {
	p, err := scanChargingPeriod(d.Pool.QueryRow(ctx, "SELECT "+chargingPeriodColumns+" FROM charging_periods ORDER BY period DESC LIMIT 1"))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChargingPeriod{}, false, nil
	}
	if err != nil {
		return ChargingPeriod{}, false, fmt.Errorf("latest charging period: %w", err)
	}
	return p, true, nil
}

// PendingChargingPeriods はファイルのない締めた月を古い順に返す。
func (d *DB) PendingChargingPeriods(ctx context.Context) ([]ChargingPeriod, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+chargingPeriodColumns+" FROM charging_periods WHERE state = 'computed' ORDER BY period")
	if err != nil {
		return nil, fmt.Errorf("pending charging periods: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargingPeriod, error) { return scanChargingPeriod(row) })
	if err != nil {
		return nil, fmt.Errorf("pending charging periods: %w", err)
	}
	return out, nil
}

// OpenLine は月末に登録されていた回線（次の月の月初の状態）。
type OpenLine struct {
	IMSI                     string
	TenantID                 int64
	VecgenState, RadiusState string
}

// OpenLines は対象月の月末に登録されていた回線を IMSI の順に、after より後ろから limit 件返す。
func (d *DB) OpenLines(ctx context.Context, month time.Time, afterIMSI string, limit int) ([]OpenLine, error) {
	rows, err := d.Pool.Query(ctx, `SELECT imsi, tenant_id, vecgen_state_end, radius_state_end FROM charging_lines
		WHERE period = $1 AND open_end AND imsi > $2 ORDER BY imsi LIMIT $3`, month, afterIMSI, limit)
	if err != nil {
		return nil, fmt.Errorf("open lines: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[OpenLine])
	if err != nil {
		return nil, fmt.Errorf("open lines: %w", err)
	}
	return out, nil
}

// ChargingLine は回線の月次の 1 行（課金レコード §7.4）。
type ChargingLine struct {
	IMSI     string
	TenantID int64
	// RegisteredMask は登録されていた日のビット（1 日が最下位）。
	RegisteredMask                                     uint32
	RegisteredDays, RadiusActiveDays, VecgenActiveDays int
	RadiusStart, RadiusEnd, VecgenStart, VecgenEnd     string
	FirstRegisteredAt, DeletedAt                       time.Time
	Usage
	UsageComplete bool
	// OpenEnd は月末にこのテナントに登録されていたか。
	OpenEnd bool
	// 読み出しのときの外部の番号（作り直すときの今の値。Z12）。
	TenantExternalID, LineExternalID string
	// TargetPeriod は訂正の行が置き換える月（訂正の行だけ）。
	TargetPeriod time.Time
}

// ChargingTenant はテナントの月次の 1 行（課金レコード §7.5）。
type ChargingTenant struct {
	TenantID                                                       int64
	Name                                                           string
	CreatedAt, DeletedAt                                           time.Time
	ExistedDays                                                    int
	LinesEnd, LinesMax, RadiusActiveLinesEnd, RadiusActiveLinesMax int
	APsEnd, GroupsEnd, BroughtCAsEnd, PlacementsEnd                int
	// LiveEnd は月末に存在していたか。
	LiveEnd bool
	// 読み出しのときの外部の顧客番号（今の値）。
	ExternalID string
}

// ErrPeriodFiled は、ファイルのある月を計算し直そうとしたこと（Z12）。
var ErrPeriodFiled = errors.New("the period already has a file")

// ChargingWriter は締めのトランザクションの中で行を書く口。
type ChargingWriter interface {
	AddLines(rows []ChargingLine) error
	AddTenants(rows []ChargingTenant) error
}

// ChargingClose は 1 つの月の締めの内容。
type ChargingClose struct {
	Month time.Time
	By    string
	At    time.Time
	// MissingDays はその月の取り込めなかった日の数。PrevMissing は前の月の取り込めなかった日のビット（訂正の計算に使う）。
	MissingDays int
	PrevMissing uint32
}

// chargingSequenceLock は通し番号を割り当てるロックの名前。
const chargingSequenceLock = "charging-sequence"

// WriteChargingPeriod は 1 つのトランザクションで月を締める。通し番号を割り当て（ファイルのない月の計算し直しは同じ番号のまま）、
// fn で月次の行を書き、前の月の行と今の集計を比べた訂正を書く（Z9・Z11）。ファイルのある月なら ErrPeriodFiled。
func (d *DB) WriteChargingPeriod(ctx context.Context, c ChargingClose, fn func(w ChargingWriter) error) (ChargingPeriod, error) {
	var out ChargingPeriod
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1::int, hashtext($2))", namedLockClass, chargingSequenceLock); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		var seq int
		var state string
		err := tx.QueryRow(ctx, "SELECT sequence, state FROM charging_periods WHERE period = $1 FOR UPDATE", c.Month).Scan(&seq, &state)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := tx.QueryRow(ctx, "SELECT coalesce(max(sequence), 0) + 1 FROM charging_periods").Scan(&seq); err != nil {
				return err
			}
		case err != nil:
			return err
		case state == PeriodFiled:
			return ErrPeriodFiled
		default:
			for _, t := range []string{"charging_lines", "charging_tenants", "charging_lines_adj"} {
				if _, err := tx.Exec(ctx, "DELETE FROM "+t+" WHERE period = $1", c.Month); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO charging_periods (period, sequence, closed_at, closed_by, missing_days)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (period) DO UPDATE SET closed_at = EXCLUDED.closed_at, closed_by = EXCLUDED.closed_by, missing_days = EXCLUDED.missing_days`,
			c.Month, seq, c.At, c.By, c.MissingDays); err != nil {
			return err
		}
		w := &chargingWriter{ctx: ctx, tx: tx, month: c.Month}
		if err := fn(w); err != nil {
			return err
		}
		adj, err := insertLineAdjustments(ctx, tx, c)
		if err != nil {
			return err
		}
		end := c.Month.AddDate(0, 1, 0)
		var events int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM charging_events WHERE time >= $1 AND time < $2", c.Month, end).Scan(&events); err != nil {
			return err
		}
		out, err = scanChargingPeriod(tx.QueryRow(ctx, `UPDATE charging_periods SET event_count = $2, line_count = $3, tenant_count = $4, adj_count = $5
			WHERE period = $1 RETURNING `+chargingPeriodColumns, c.Month, events, w.lines, w.tenants, adj))
		return err
	})
	if errors.Is(err, ErrPeriodFiled) {
		return ChargingPeriod{}, ErrPeriodFiled
	}
	if err != nil {
		return ChargingPeriod{}, fmt.Errorf("write charging period %s: %w", c.Month.Format("2006-01"), err)
	}
	return out, nil
}

// chargingWriter は WriteChargingPeriod のトランザクションの中で書く。
type chargingWriter struct {
	ctx            context.Context
	tx             pgx.Tx
	month          time.Time
	lines, tenants int
}

func (w *chargingWriter) AddLines(rows []ChargingLine) error {
	n, err := w.tx.CopyFrom(w.ctx, pgx.Identifier{"charging_lines"}, []string{"period", "imsi", "tenant_id", "registered_mask", "registered_days",
		"radius_active_days", "vecgen_active_days", "radius_state_start", "radius_state_end", "vecgen_state_start", "vecgen_state_end",
		"first_registered_at", "deleted_at", "auth_ok", "sessions", "seconds", "in_octets", "out_octets", "usage_complete", "open_end"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			r := rows[i]
			return []any{w.month, r.IMSI, r.TenantID, int32(r.RegisteredMask), int16(r.RegisteredDays), int16(r.RadiusActiveDays), int16(r.VecgenActiveDays),
				r.RadiusStart, r.RadiusEnd, r.VecgenStart, r.VecgenEnd, nullTime(r.FirstRegisteredAt), nullTime(r.DeletedAt),
				r.AuthOK, r.Sessions, r.Seconds, r.InOctets, r.OutOctets, r.UsageComplete, r.OpenEnd}, nil
		}))
	if err != nil {
		return fmt.Errorf("add charging lines: %w", err)
	}
	w.lines += int(n)
	return nil
}

func (w *chargingWriter) AddTenants(rows []ChargingTenant) error {
	n, err := w.tx.CopyFrom(w.ctx, pgx.Identifier{"charging_tenants"}, []string{"period", "tenant_id", "tenant_name", "created_at", "deleted_at", "existed_days",
		"lines_end", "lines_max", "radius_active_lines_end", "radius_active_lines_max", "aps_end", "groups_end", "brought_cas_end", "placements_end", "live_end"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			r := rows[i]
			return []any{w.month, r.TenantID, r.Name, nullTime(r.CreatedAt), nullTime(r.DeletedAt), int16(r.ExistedDays), int32(r.LinesEnd), int32(r.LinesMax),
				int32(r.RadiusActiveLinesEnd), int32(r.RadiusActiveLinesMax), int32(r.APsEnd), int32(r.GroupsEnd), int32(r.BroughtCAsEnd), int32(r.PlacementsEnd), r.LiveEnd}, nil
		}))
	if err != nil {
		return fmt.Errorf("add charging tenants: %w", err)
	}
	w.tenants += int(n)
	return nil
}

// insertLineAdjustments は、前の月の行と今の集計を比べ、違う行だけ全体を訂正の行として書く（元の行は変えない。Z11）。
// 前の月を締めていなければ何もしない。
func insertLineAdjustments(ctx context.Context, tx pgx.Tx, c ChargingClose) (int, error) {
	prev := c.Month.AddDate(0, -1, 0)
	tag, err := tx.Exec(ctx, `
		INSERT INTO charging_lines_adj (period, target_period, imsi, tenant_id, registered_days, radius_active_days, vecgen_active_days,
			radius_state_start, radius_state_end, vecgen_state_start, vecgen_state_end, first_registered_at, deleted_at,
			auth_ok, sessions, seconds, in_octets, out_octets, usage_complete, reason)
		SELECT $1, $2, coalesce(l.imsi, u.imsi), coalesce(l.tenant_id, u.tenant_id), coalesce(l.registered_days, 0), coalesce(l.radius_active_days, 0),
			coalesce(l.vecgen_active_days, 0), coalesce(l.radius_state_start, ''), coalesce(l.radius_state_end, ''), coalesce(l.vecgen_state_start, ''),
			coalesce(l.vecgen_state_end, ''), l.first_registered_at, l.deleted_at, coalesce(u.auth_ok, 0), coalesce(u.sessions, 0), coalesce(u.seconds, 0),
			coalesce(u.in_octets, 0), coalesce(u.out_octets, 0), (coalesce(l.registered_mask, 0) & $4) = 0, 'late_usage'
		FROM (SELECT * FROM charging_lines WHERE period = $2) l
		FULL JOIN (SELECT imsi, tenant_id, sum(auth_ok)::bigint AS auth_ok, sum(sessions)::bigint AS sessions, sum(seconds)::bigint AS seconds,
				sum(in_octets)::bigint AS in_octets, sum(out_octets)::bigint AS out_octets
			FROM usage_daily WHERE date >= $2 AND date < $3 GROUP BY imsi, tenant_id) u
			ON l.imsi = u.imsi AND l.tenant_id = u.tenant_id
		WHERE EXISTS (SELECT 1 FROM charging_periods WHERE period = $2) AND (
			(coalesce(l.auth_ok, 0), coalesce(l.sessions, 0), coalesce(l.seconds, 0), coalesce(l.in_octets, 0), coalesce(l.out_octets, 0))
				IS DISTINCT FROM (coalesce(u.auth_ok, 0), coalesce(u.sessions, 0), coalesce(u.seconds, 0), coalesce(u.in_octets, 0), coalesce(u.out_octets, 0))
			OR coalesce(l.usage_complete, true) IS DISTINCT FROM ((coalesce(l.registered_mask, 0) & $4) = 0))`,
		c.Month, prev, c.Month, int32(c.PrevMissing))
	if err != nil {
		return 0, fmt.Errorf("line adjustments: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ChargingLinesPage は対象月の回線の月次を (IMSI, テナント) の順に、after より後ろから limit 件返す（外部の番号は今の値）。
func (d *DB) ChargingLinesPage(ctx context.Context, month time.Time, afterIMSI string, afterTenant int64, limit int) ([]ChargingLine, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT l.imsi, l.tenant_id, l.registered_mask, l.registered_days, l.radius_active_days, l.vecgen_active_days,
			l.radius_state_start, l.radius_state_end, l.vecgen_state_start, l.vecgen_state_end, l.first_registered_at, l.deleted_at,
			l.auth_ok, l.sessions, l.seconds, l.in_octets, l.out_octets, l.usage_complete, l.open_end,
			coalesce(t.external_customer_id, ''), coalesce(s.external_line_id, '')
		FROM charging_lines l
		LEFT JOIN tenants t ON t.id = l.tenant_id
		LEFT JOIN subscribers s ON s.imsi = l.imsi
		WHERE l.period = $1 AND (l.imsi, l.tenant_id) > ($2, $3) ORDER BY l.imsi, l.tenant_id LIMIT $4`, month, afterIMSI, afterTenant, limit)
	if err != nil {
		return nil, fmt.Errorf("charging lines: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargingLine, error) {
		var l ChargingLine
		var mask int32
		var first, deleted *time.Time
		err := row.Scan(&l.IMSI, &l.TenantID, &mask, &l.RegisteredDays, &l.RadiusActiveDays, &l.VecgenActiveDays, &l.RadiusStart, &l.RadiusEnd,
			&l.VecgenStart, &l.VecgenEnd, &first, &deleted, &l.AuthOK, &l.Sessions, &l.Seconds, &l.InOctets, &l.OutOctets, &l.UsageComplete, &l.OpenEnd,
			&l.TenantExternalID, &l.LineExternalID)
		l.RegisteredMask = uint32(mask)
		if first != nil {
			l.FirstRegisteredAt = *first
		}
		if deleted != nil {
			l.DeletedAt = *deleted
		}
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("charging lines: %w", err)
	}
	return out, nil
}

// ChargingAdjPage は、対象月に出す訂正の行を (IMSI, テナント) の順に、after より後ろから limit 件返す（外部の番号は今の値）。
func (d *DB) ChargingAdjPage(ctx context.Context, month time.Time, afterIMSI string, afterTenant int64, limit int) ([]ChargingLine, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT l.imsi, l.tenant_id, l.target_period, l.registered_days, l.radius_active_days, l.vecgen_active_days,
			l.radius_state_start, l.radius_state_end, l.vecgen_state_start, l.vecgen_state_end, l.first_registered_at, l.deleted_at,
			l.auth_ok, l.sessions, l.seconds, l.in_octets, l.out_octets, l.usage_complete,
			coalesce(t.external_customer_id, ''), coalesce(s.external_line_id, '')
		FROM charging_lines_adj l
		LEFT JOIN tenants t ON t.id = l.tenant_id
		LEFT JOIN subscribers s ON s.imsi = l.imsi
		WHERE l.period = $1 AND (l.imsi, l.tenant_id) > ($2, $3) ORDER BY l.imsi, l.tenant_id LIMIT $4`, month, afterIMSI, afterTenant, limit)
	if err != nil {
		return nil, fmt.Errorf("charging adjustments: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargingLine, error) {
		var l ChargingLine
		var first, deleted *time.Time
		err := row.Scan(&l.IMSI, &l.TenantID, &l.TargetPeriod, &l.RegisteredDays, &l.RadiusActiveDays, &l.VecgenActiveDays, &l.RadiusStart, &l.RadiusEnd,
			&l.VecgenStart, &l.VecgenEnd, &first, &deleted, &l.AuthOK, &l.Sessions, &l.Seconds, &l.InOctets, &l.OutOctets, &l.UsageComplete,
			&l.TenantExternalID, &l.LineExternalID)
		l.TargetPeriod = asDate(l.TargetPeriod)
		if first != nil {
			l.FirstRegisteredAt = *first
		}
		if deleted != nil {
			l.DeletedAt = *deleted
		}
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("charging adjustments: %w", err)
	}
	return out, nil
}

// ChargingTenantsOf は対象月のテナントの月次をテナントの順に返す（外部の顧客番号は今の値）。
func (d *DB) ChargingTenantsOf(ctx context.Context, month time.Time) ([]ChargingTenant, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT c.tenant_id, c.tenant_name, c.created_at, c.deleted_at, c.existed_days, c.lines_end, c.lines_max, c.radius_active_lines_end,
			c.radius_active_lines_max, c.aps_end, c.groups_end, c.brought_cas_end, c.placements_end, c.live_end, coalesce(t.external_customer_id, '')
		FROM charging_tenants c LEFT JOIN tenants t ON t.id = c.tenant_id
		WHERE c.period = $1 ORDER BY c.tenant_id`, month)
	if err != nil {
		return nil, fmt.Errorf("charging tenants: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargingTenant, error) {
		var c ChargingTenant
		var created, deleted *time.Time
		err := row.Scan(&c.TenantID, &c.Name, &created, &deleted, &c.ExistedDays, &c.LinesEnd, &c.LinesMax, &c.RadiusActiveLinesEnd, &c.RadiusActiveLinesMax,
			&c.APsEnd, &c.GroupsEnd, &c.BroughtCAsEnd, &c.PlacementsEnd, &c.LiveEnd, &c.ExternalID)
		if created != nil {
			c.CreatedAt = *created
		}
		if deleted != nil {
			c.DeletedAt = *deleted
		}
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("charging tenants: %w", err)
	}
	return out, nil
}

// ChargingEventRow は events.csv の 1 行（外部の番号は今の値）。
type ChargingEventRow struct {
	ChargingEvent
	TenantExternalID, LineExternalID string
}

// ChargingEventsPage は [start, end) の事実を番号の順に、after より後ろから limit 件返す。
func (d *DB) ChargingEventsPage(ctx context.Context, start, end time.Time, afterID int64, limit int) ([]ChargingEventRow, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT e.id, e.time, e.event, coalesce(e.side, ''), e.tenant_id, coalesce(e.imsi, ''), e.from_val, e.to_val, e.operator, e.job_id, e.trace_id, e.reason,
			coalesce(t.external_customer_id, ''), coalesce(s.external_line_id, '')
		FROM charging_events e
		LEFT JOIN tenants t ON t.id = e.tenant_id
		LEFT JOIN subscribers s ON s.imsi = e.imsi
		WHERE e.time >= $1 AND e.time < $2 AND e.id > $3 ORDER BY e.id LIMIT $4`, start, end, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("charging events page: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargingEventRow, error) {
		var ev ChargingEventRow
		err := row.Scan(&ev.ID, &ev.Time, &ev.Event, &ev.Side, &ev.TenantID, &ev.IMSI, &ev.From, &ev.To, &ev.Operator, &ev.JobID, &ev.TraceID, &ev.Reason,
			&ev.TenantExternalID, &ev.LineExternalID)
		return ev, err
	})
	if err != nil {
		return nil, fmt.Errorf("charging events page: %w", err)
	}
	return out, nil
}

// ---- ファイル ----

// ChargingFile は月次のまとめのファイル（Data は読み出しの口でだけ入れる）。
type ChargingFile struct {
	ID                                            int64
	Period                                        time.Time
	Version                                       int
	FileName, KeyFingerprint, ZipSHA256, Manifest string
	EventCount, LineCount, TenantCount, AdjCount  int
	Size                                          int64
	CreatedAt                                     time.Time
	CreatedBy                                     string
	Data                                          []byte
}

const chargingFileColumns = `id, period, version, file_name, key_fingerprint, zip_sha256, manifest, event_count, line_count, tenant_count, adj_count, size, created_at, created_by`

func scanChargingFile(row pgx.Row, extra ...any) (ChargingFile, error) {
	var f ChargingFile
	dest := append([]any{&f.ID, &f.Period, &f.Version, &f.FileName, &f.KeyFingerprint, &f.ZipSHA256, &f.Manifest, &f.EventCount, &f.LineCount, &f.TenantCount,
		&f.AdjCount, &f.Size, &f.CreatedAt, &f.CreatedBy}, extra...)
	err := row.Scan(dest...)
	f.Period = asDate(f.Period)
	return f, err
}

// NextChargingFileVersion はその月の次の版を返す。
func (d *DB) NextChargingFileVersion(ctx context.Context, month time.Time) (int, error) {
	var v int
	if err := d.Pool.QueryRow(ctx, "SELECT coalesce(max(version), 0) + 1 FROM charging_files WHERE period = $1", month).Scan(&v); err != nil {
		return 0, fmt.Errorf("next charging file version: %w", err)
	}
	return v, nil
}

// InsertChargingFile はファイルを書き、その月をファイルのある月にする。
func (d *DB) InsertChargingFile(ctx context.Context, f ChargingFile) (ChargingFile, error) {
	var out ChargingFile
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanChargingFile(tx.QueryRow(ctx, `
			INSERT INTO charging_files (period, version, file_name, key_fingerprint, zip_sha256, manifest, event_count, line_count, tenant_count, adj_count, size, created_at, created_by, data)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING `+chargingFileColumns,
			f.Period, f.Version, f.FileName, f.KeyFingerprint, f.ZipSHA256, f.Manifest, f.EventCount, f.LineCount, f.TenantCount, f.AdjCount, int64(len(f.Data)), f.CreatedAt, f.CreatedBy, f.Data))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE charging_periods SET state = 'filed' WHERE period = $1", f.Period)
		return err
	})
	if err != nil {
		return ChargingFile{}, fmt.Errorf("insert charging file: %w", err)
	}
	return out, nil
}

// ChargingFiles はファイルの一覧を、月の新しい順・版の新しい順に返す（中身は読まない）。
func (d *DB) ChargingFiles(ctx context.Context) ([]ChargingFile, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+chargingFileColumns+" FROM charging_files ORDER BY period DESC, version DESC")
	if err != nil {
		return nil, fmt.Errorf("charging files: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ChargingFile, error) { return scanChargingFile(row) })
	if err != nil {
		return nil, fmt.Errorf("charging files: %w", err)
	}
	return out, nil
}

// ChargingFileData はファイルを中身つきで返す。なければ ErrNotFound。
func (d *DB) ChargingFileData(ctx context.Context, id int64) (ChargingFile, error) {
	var data []byte
	f, err := scanChargingFile(d.Pool.QueryRow(ctx, "SELECT "+chargingFileColumns+", data FROM charging_files WHERE id = $1", id), &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChargingFile{}, ErrNotFound
	}
	if err != nil {
		return ChargingFile{}, fmt.Errorf("charging file: %w", err)
	}
	f.Data = data
	return f, nil
}

// ---- PGP の公開鍵 ----

// PGPKey は課金レコードを暗号化する PGP の公開鍵。
type PGPKey struct {
	ID                              int64
	Armored, Fingerprint, Algorithm string
	// ExpiresAt は期限（ゼロ値は無期限）。
	ExpiresAt time.Time
	CreatedAt time.Time
	CreatedBy string
}

func scanPGPKey(row pgx.Row) (PGPKey, error) {
	var k PGPKey
	var exp *time.Time
	err := row.Scan(&k.ID, &k.Armored, &k.Fingerprint, &k.Algorithm, &exp, &k.CreatedAt, &k.CreatedBy)
	if exp != nil {
		k.ExpiresAt = *exp
	}
	return k, err
}

const pgpKeyColumns = `id, armored, fingerprint, algorithm, expires_at, created_at, created_by`

// InsertPGPKey は公開鍵を今の鍵にする（前の鍵は履歴として残る）。
func (d *DB) InsertPGPKey(ctx context.Context, k PGPKey) (PGPKey, error) {
	out, err := scanPGPKey(d.Pool.QueryRow(ctx, `INSERT INTO charging_pgp_keys (armored, fingerprint, algorithm, expires_at, created_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+pgpKeyColumns, k.Armored, k.Fingerprint, k.Algorithm, nullTime(k.ExpiresAt), k.CreatedAt, k.CreatedBy))
	if err != nil {
		return PGPKey{}, fmt.Errorf("insert pgp key: %w", err)
	}
	return out, nil
}

// CurrentPGPKey は今の鍵を返す（なければ false）。
func (d *DB) CurrentPGPKey(ctx context.Context) (PGPKey, bool, error) {
	k, err := scanPGPKey(d.Pool.QueryRow(ctx, "SELECT "+pgpKeyColumns+" FROM charging_pgp_keys ORDER BY id DESC LIMIT 1"))
	if errors.Is(err, pgx.ErrNoRows) {
		return PGPKey{}, false, nil
	}
	if err != nil {
		return PGPKey{}, false, fmt.Errorf("current pgp key: %w", err)
	}
	return k, true, nil
}

// PGPKeys は鍵の履歴を新しい順に返す。
func (d *DB) PGPKeys(ctx context.Context, limit int) ([]PGPKey, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+pgpKeyColumns+" FROM charging_pgp_keys ORDER BY id DESC LIMIT $1", limit)
	if err != nil {
		return nil, fmt.Errorf("pgp keys: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PGPKey, error) { return scanPGPKey(row) })
	if err != nil {
		return nil, fmt.Errorf("pgp keys: %w", err)
	}
	return out, nil
}

// ---- 外部の番号 ----

// SetLineExternalID は回線の外部の番号を変え、変える前の値を返す。台帳にない IMSI なら ErrNotFound。
func (d *DB) SetLineExternalID(ctx context.Context, imsi, value string) (string, error) {
	var before string
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT external_line_id FROM subscribers WHERE imsi = $1 FOR UPDATE", imsi).Scan(&before); err != nil {
			return err
		}
		if before == value {
			return nil
		}
		_, err := tx.Exec(ctx, "UPDATE subscribers SET external_line_id = $2, updated_at = now() WHERE imsi = $1", imsi, value)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("set line external id: %w", err)
	}
	return before, nil
}

// ---- 13 か月より古いものの削除 ----

// dropOldUsagePartitions は before の月より古い月の usage_daily のパーティションを消し、消した数を返す。
func (d *DB) dropOldUsagePartitions(ctx context.Context, before time.Time) (int64, error) {
	rows, err := d.Pool.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'usage_daily'::regclass ORDER BY 1`)
	if err != nil {
		return 0, fmt.Errorf("list usage partitions: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("list usage partitions: %w", err)
	}
	limit := usagePartition(MonthOf(before))
	var n int64
	for _, name := range names {
		// 名前は usage_daily_YYYYMM なので、文字列の順が月の順になる。
		if name >= limit {
			break
		}
		if _, err := d.Pool.Exec(ctx, "DROP TABLE "+pgx.Identifier{name}.Sanitize()); err != nil {
			return n, fmt.Errorf("drop usage partition %s: %w", name, err)
		}
		n++
	}
	return n, nil
}
