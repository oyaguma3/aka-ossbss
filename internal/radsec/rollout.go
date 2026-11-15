package radsec

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// エッジのコンテナのひな形を段階的に届ける「反映し直し」（aka-mvpset の段階 6-0 の U6-8。radsec-edge の §24.14）。
// 運用担当・システム管理者が選んだテナントを、エッジごとに 1 件ずつ今の形・前の形で反映し直し、1 件でも成功しなければ残りに送らない。

// rolloutResultWait は、反映を始めてから結果が読めないまま待つ上限（エッジが反映の途中で止まって結果を残さなかったときなど）。
const rolloutResultWait = 10 * time.Minute

// writeTemplate はエッジが返すひな形の状態の写しを配置に書く（変わったときだけ）。
func (p *pass) writeTemplate(ctx context.Context, st *placementState) {
	if st.ev.down || st.s == nil {
		return
	}
	pl, tpl := st.pl, st.s.Template
	can := tpl.CanRollback != nil && *tpl.CanRollback
	image := ""
	if tpl.Recorded != nil {
		image = shortImage(tpl.Recorded.Image)
	}
	if sameBool(pl.TemplateCurrent, tpl.Current) && sameBool(pl.ConfigCurrent, st.s.ConfigCurrent) && pl.CanRollback == can && pl.TemplateImage == image {
		return
	}
	if err := p.r.opts.DB.SetPlacementTemplate(ctx, pl.ID, tpl.Current, st.s.ConfigCurrent, can, image); err != nil {
		p.r.opts.Log.Warn("radsec: write placement template", "event_id", "RADSEC_SYNC_ERR", "placement", pl.ID, "error", err)
	}
}

func sameBool(a, b *bool) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// shortImage はイメージの ID の短い形（sha256: を除いた 12 桁）。
func shortImage(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(12, len(id))]
}

// rollouts は動いている反映し直しを 1 件ずつ進める。エッジの一覧を読んだ後、テナントごとの同期の前に呼ぶ
// （反映し直しで始めた反映が動いている間は、普段の反映と宣言をそのテナントに送らない）。
func (p *pass) rollouts(ctx context.Context) {
	rs, err := p.r.opts.DB.RunningRollouts(ctx)
	if err != nil {
		p.r.opts.Log.Warn("radsec: read rollouts", "event_id", "RADSEC_SYNC_ERR", "error", err)
		return
	}
	for _, r := range rs {
		if ctx.Err() != nil {
			return
		}
		ev := p.edges[r.EdgeID]
		if ev == nil || ev.down {
			continue
		}
		p.stepRollout(ctx, r, ev)
	}
}

// stepRollout は 1 つの反映し直しの今の 1 件を進める。
func (p *pass) stepRollout(ctx context.Context, r db.Rollout, ev *edgeView) {
	var it *db.RolloutItem
	for i := range r.Items {
		if s := r.Items[i].State; s == db.RolloutItemWaiting || s == db.RolloutItemApplying {
			it = &r.Items[i]
			break
		}
	}
	if it == nil {
		if err := p.r.opts.DB.FinishRollout(ctx, r.ID, db.RolloutDone, ""); err != nil {
			p.r.opts.Log.Warn("radsec: finish rollout", "event_id", "RADSEC_SYNC_ERR", "rollout", r.ID, "error", err)
			return
		}
		p.r.opts.Log.Info("radsec: rollout finished", "rollout", r.ID, "edge", r.EdgeID, "mode", r.Mode, "ok", r.Count(db.RolloutItemOK), "skipped", r.Count(db.RolloutItemSkipped))
		p.record(ctx, audit.ActionLoopRolloutFinish, r.EdgeID, 0, map[string]any{"rollout": r.ID, "edge": r.EdgeID, "mode": r.Mode, "ok": r.Count(db.RolloutItemOK)})
		return
	}
	s := ev.tenants[it.TenantID]
	switch it.State {
	case db.RolloutItemWaiting:
		switch {
		case s == nil:
			// エッジにまだない・外した配置。反映し直すものがない。
			p.setItem(ctx, r, it, db.RolloutItemSkipped, time.Time{}, "エッジにテナントがありません")
			p.busy = true
			return
		case !s.Enabled:
			p.setItem(ctx, r, it, db.RolloutItemSkipped, time.Time{}, "エッジの画面でテナントを止めています")
			p.busy = true
			return
		case s.Apply.State == stateRunning:
			p.busy = true // 普段の反映が動いている。終わってから始める
			return
		}
		key := fmt.Sprintf("radsec-rollout%d-%d-%s", r.ID, it.Seq, rand.Text())
		a, err := p.r.opts.PV.ApplyEdgeTenantTemplate(ctx, r.EdgeID, it.TenantID, r.Mode, key)
		switch {
		case err == nil && !a.Started:
			p.busy = true // ちょうど別の反映が動いていた。次の周回でもう一度
			return
		case err == nil:
			started := a.StartedAt
			if started.IsZero() {
				started = p.now
			}
			p.setItem(ctx, r, it, db.RolloutItemApplying, started, "")
			s.Apply = provisioner.EdgeApply{State: stateRunning}
			ev.running++
			p.busy = true
		case provisioner.CauseOf(err) == provisioner.CauseTenantBusy:
			p.busy = true
		case provisioner.ResultUnknown(err):
			p.markDown(ev, err)
		default:
			p.stopRollout(ctx, r, it, describe(err))
		}
	case db.RolloutItemApplying:
		switch {
		case s == nil:
			p.stopRollout(ctx, r, it, "エッジにテナントがなくなりました")
			return
		case s.Apply.State == stateRunning:
			p.busy = true
			return
		case s.Apply.LastFinishedAt.Before(it.StartedAt):
			// 始めた反映の結果がまだない（エッジが途中で止まったなど）。しばらく待ってから止める。
			if p.now.Sub(it.StartedAt) > rolloutResultWait {
				p.stopRollout(ctx, r, it, "反映の結果が分かりません（エッジの反映の履歴を確かめてください）")
				return
			}
			p.busy = true
			return
		}
		switch s.Apply.LastResult {
		case resultOK:
			p.setItem(ctx, r, it, db.RolloutItemOK, time.Time{}, "")
			p.busy = true // 次の 1 件はすぐ次の周回で
		case "interrupted":
			// radsec-edge を止めたので取り消された。前の形のまま（前の版に戻した）なので、もう一度始める。
			p.setItem(ctx, r, it, db.RolloutItemWaiting, time.Time{}, "")
			p.busy = true
		default:
			msg := s.Apply.LastError
			if msg == "" {
				msg = "反映に失敗しました（" + s.Apply.LastResult + "）"
			}
			p.stopRollout(ctx, r, it, msg)
		}
	}
}

// setItem は 1 件の状態を書いて監査ログに残す（始めたときと終わったとき）。
func (p *pass) setItem(ctx context.Context, r db.Rollout, it *db.RolloutItem, state string, started time.Time, errText string) {
	var finished time.Time
	if state == db.RolloutItemOK || state == db.RolloutItemFailed || state == db.RolloutItemSkipped {
		finished = p.now
	}
	if err := p.r.opts.DB.SetRolloutItem(ctx, r.ID, it.Seq, state, started, finished, truncate(errText)); err != nil {
		p.r.opts.Log.Warn("radsec: write rollout item", "event_id", "RADSEC_SYNC_ERR", "rollout", r.ID, "seq", it.Seq, "error", err)
		return
	}
	it.State = state
	p.record(ctx, audit.ActionLoopRolloutItem, r.EdgeID, it.TenantID, map[string]any{"rollout": r.ID, "edge": r.EdgeID, "mode": r.Mode, "seq": it.Seq, "state": state, "error": truncate(errText)})
}

// stopRollout は 1 件を失敗にして反映し直しを止める（残りに送らない。人が続けるか取り消す）。
func (p *pass) stopRollout(ctx context.Context, r db.Rollout, it *db.RolloutItem, reason string) {
	p.setItem(ctx, r, it, db.RolloutItemFailed, time.Time{}, reason)
	if err := p.r.opts.DB.FinishRollout(ctx, r.ID, db.RolloutStopped, truncate(reason)); err != nil {
		p.r.opts.Log.Warn("radsec: stop rollout", "event_id", "RADSEC_SYNC_ERR", "rollout", r.ID, "error", err)
		return
	}
	p.r.opts.Log.Warn("radsec: rollout stopped", "event_id", "RADSEC_ROLLOUT_STOPPED", "rollout", r.ID, "edge", r.EdgeID, "mode", r.Mode,
		"tenant", it.TenantID, "seq", it.Seq, "reason", reason)
	p.record(ctx, audit.ActionLoopRolloutStop, r.EdgeID, it.TenantID, map[string]any{"rollout": r.ID, "edge": r.EdgeID, "mode": r.Mode, "seq": it.Seq, "reason": truncate(reason)})
}
