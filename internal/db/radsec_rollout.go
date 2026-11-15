package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// エッジのコンテナのひな形を段階的に届ける「反映し直し」（aka-mvpset の段階 6-0 の U6-8。移行 0007）。

// 反映し直しの状態。
const (
	RolloutRunning  = "running"
	RolloutStopped  = "stopped"
	RolloutDone     = "done"
	RolloutCanceled = "canceled"
)

// 反映し直しの 1 件の状態。
const (
	RolloutItemWaiting  = "waiting"
	RolloutItemApplying = "applying"
	RolloutItemOK       = "ok"
	RolloutItemFailed   = "failed"
	RolloutItemSkipped  = "skipped"
)

// ErrRolloutActive は、同じエッジに動いている・止まっている反映し直しがあることを表す。
var ErrRolloutActive = errors.New("a rollout is already active on the edge")

// Rollout は反映し直し。Mode は latest（今の形）か previous（前の形）。
type Rollout struct {
	ID         int64
	EdgeID     string
	Mode       string
	State      string
	CreatedAt  time.Time
	CreatedBy  string
	FinishedAt time.Time
	StopReason string
	Items      []RolloutItem
}

// Active は動いているか止まっているか（同じエッジに 1 つまで）。
func (r Rollout) Active() bool { return r.State == RolloutRunning || r.State == RolloutStopped }

// Count は状態ごとの件数を返す。
func (r Rollout) Count(state string) int {
	n := 0
	for _, it := range r.Items {
		if it.State == state {
			n++
		}
	}
	return n
}

// RolloutItem は反映し直しの 1 件。StartedAt はエッジが返した反映の始まりの時刻（エッジの時計）。
type RolloutItem struct {
	Seq         int
	PlacementID int64
	TenantID    int64
	State       string
	StartedAt   time.Time
	FinishedAt  time.Time
	Error       string
}

// SetPlacementTemplate は配置のひな形の状態の写しを書く。
func (d *DB) SetPlacementTemplate(ctx context.Context, id int64, current, config *bool, canRollback bool, image string) error {
	if _, err := d.Pool.Exec(ctx, `UPDATE placements SET template_current = $2, config_current = $3, can_rollback = $4, template_image = $5 WHERE id = $1`,
		id, current, config, canRollback, image); err != nil {
		return fmt.Errorf("set placement template: %w", err)
	}
	return nil
}

// CreateRollout は反映し直しを作る。items は選んだ順の配置（同じエッジのもの）。同じエッジに動いている・止まっているものがあれば ErrRolloutActive。
func (d *DB) CreateRollout(ctx context.Context, edge, mode, by string, items []Placement) (Rollout, error) {
	var out Rollout
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO edge_rollouts (edge_id, mode, state, created_by) VALUES ($1, $2, 'running', $3) RETURNING id, created_at`,
			edge, mode, by).Scan(&out.ID, &out.CreatedAt)
		if pgErrorCode(err) == pgUniqueViolation {
			return ErrRolloutActive
		}
		if err != nil {
			return err
		}
		for i, p := range items {
			if p.EdgeID != edge {
				return fmt.Errorf("placement %d is not on edge %s", p.ID, edge)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO edge_rollout_items (rollout_id, seq, placement_id, tenant_id, state) VALUES ($1, $2, $3, $4, 'waiting')`,
				out.ID, i+1, p.ID, p.TenantID); err != nil {
				return err
			}
			out.Items = append(out.Items, RolloutItem{Seq: i + 1, PlacementID: p.ID, TenantID: p.TenantID, State: RolloutItemWaiting})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrRolloutActive) {
			return Rollout{}, err
		}
		return Rollout{}, fmt.Errorf("create rollout: %w", err)
	}
	out.EdgeID, out.Mode, out.State, out.CreatedBy = edge, mode, RolloutRunning, by
	return out, nil
}

const rolloutColumns = `id, edge_id, mode, state, created_at, created_by, finished_at, stop_reason`

// rollouts は条件 where（$1 からの引数 args）に合う反映し直しを、1 件ずつの項目と一緒に新しい順に返す。
func (d *DB) rollouts(ctx context.Context, where string, args ...any) ([]Rollout, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+rolloutColumns+" FROM edge_rollouts WHERE "+where+" ORDER BY id DESC", args...)
	if err != nil {
		return nil, fmt.Errorf("rollouts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Rollout, error) {
		var r Rollout
		var finished *time.Time
		err := row.Scan(&r.ID, &r.EdgeID, &r.Mode, &r.State, &r.CreatedAt, &r.CreatedBy, &finished, &r.StopReason)
		if finished != nil {
			r.FinishedAt = *finished
		}
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("rollouts: %w", err)
	}
	for i := range out {
		rows, err := d.Pool.Query(ctx, `SELECT seq, placement_id, tenant_id, state, started_at, finished_at, error FROM edge_rollout_items WHERE rollout_id = $1 ORDER BY seq`, out[i].ID)
		if err != nil {
			return nil, fmt.Errorf("rollout items: %w", err)
		}
		out[i].Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RolloutItem, error) {
			var it RolloutItem
			var started, finished *time.Time
			err := row.Scan(&it.Seq, &it.PlacementID, &it.TenantID, &it.State, &started, &finished, &it.Error)
			if started != nil {
				it.StartedAt = *started
			}
			if finished != nil {
				it.FinishedAt = *finished
			}
			return it, err
		})
		if err != nil {
			return nil, fmt.Errorf("rollout items: %w", err)
		}
	}
	return out, nil
}

// RunningRollouts は動いている反映し直しを返す（整合のループが進める）。
func (d *DB) RunningRollouts(ctx context.Context) ([]Rollout, error) {
	return d.rollouts(ctx, "state = 'running'")
}

// EdgeRollouts はエッジの反映し直しを新しい順に limit 件返す（動いている・止まっているものが先頭になる）。
func (d *DB) EdgeRollouts(ctx context.Context, edge string, limit int) ([]Rollout, error) {
	return d.rollouts(ctx, "edge_id = $1 AND id IN (SELECT id FROM edge_rollouts WHERE edge_id = $1 ORDER BY (state IN ('running', 'stopped')) DESC, id DESC LIMIT $2)", edge, limit)
}

// StoppedRollouts は止まっている反映し直しを返す（ダッシュボード）。
func (d *DB) StoppedRollouts(ctx context.Context) ([]Rollout, error) {
	return d.rollouts(ctx, "state = 'stopped'")
}

// SetRolloutItem は 1 件の状態を書く。started・finished はゼロ値なら変えない。
func (d *DB) SetRolloutItem(ctx context.Context, rollout int64, seq int, state string, started, finished time.Time, errText string) error {
	var s, f *time.Time
	if !started.IsZero() {
		s = &started
	}
	if !finished.IsZero() {
		f = &finished
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE edge_rollout_items SET state = $3, started_at = coalesce($4, started_at), finished_at = coalesce($5, finished_at), error = $6
		WHERE rollout_id = $1 AND seq = $2`, rollout, seq, state, s, f, errText); err != nil {
		return fmt.Errorf("set rollout item: %w", err)
	}
	return nil
}

// FinishRollout は動いている反映し直しを終える（done）か止める（stopped。reason は理由）。
func (d *DB) FinishRollout(ctx context.Context, rollout int64, state, reason string) error {
	if _, err := d.Pool.Exec(ctx, `UPDATE edge_rollouts SET state = $2, stop_reason = $3, finished_at = CASE WHEN $2 = 'done' THEN now() ELSE finished_at END
		WHERE id = $1 AND state = 'running'`, rollout, state, reason); err != nil {
		return fmt.Errorf("finish rollout: %w", err)
	}
	return nil
}

// ResumeRollout は止まっている反映し直しを続ける（失敗した 1 件を待ちに戻す）。なければ ErrNotFound。
func (d *DB) ResumeRollout(ctx context.Context, edge string, rollout int64) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE edge_rollouts SET state = 'running', stop_reason = '' WHERE id = $1 AND edge_id = $2 AND state = 'stopped'`, rollout, edge)
		if err != nil {
			return fmt.Errorf("resume rollout: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `UPDATE edge_rollout_items SET state = 'waiting', started_at = NULL, finished_at = NULL, error = '' WHERE rollout_id = $1 AND state = 'failed'`, rollout)
		return err
	})
}

// CancelRollout は動いている・止まっている反映し直しを取り消す（まだの項目は飛ばす）。動いている 1 件の反映はエッジで続く。なければ ErrNotFound。
func (d *DB) CancelRollout(ctx context.Context, edge string, rollout int64) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE edge_rollouts SET state = 'canceled', finished_at = now() WHERE id = $1 AND edge_id = $2 AND state IN ('running', 'stopped')`, rollout, edge)
		if err != nil {
			return fmt.Errorf("cancel rollout: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `UPDATE edge_rollout_items SET state = 'skipped' WHERE rollout_id = $1 AND state IN ('waiting', 'failed')`, rollout)
		return err
	})
}
