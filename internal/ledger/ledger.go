// Package ledger は加入者の 1 回の操作の手順（設計概要 §7.2・§8。段階 3-1 の W6・W7）。画面の操作と一括投入のジョブ（段階 3-2 の X8）が
// 同じ手順を使う。IMSI ごとのロックを取ったトランザクションの中で、台帳を確かめ、aka-provisioner を 1 回呼び、成功したら台帳と
// 状態の事実を書く。結果が分からない（503・502・タイムアウトなど）ときは、台帳を変えずに印（check_needed）を立て、その場で両側を
// 一括取得で読み直して台帳を直す（補いの事実 reconciled を書く）。読み直せなければ印を残す。
package ledger

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// ReasonUnknown は、結果が分からなかった操作を読み直して台帳を直したときの、補いの事実の理由。
const ReasonUnknown = "結果が分からなかった操作の確かめ"

// DB は手順が使う DB の操作。*db.DB が満たす。
type DB interface {
	WithSubscriber(ctx context.Context, imsi string, meta db.Meta, fn func(db.Ledger) error) error
}

// Provisioner は読み直しに使う aka-provisioner の口。*provisioner.Client が満たす。
type Provisioner interface {
	VecgenBatchGet(ctx context.Context, imsis []string) (provisioner.VecgenBatch, error)
	RadiusBatchGet(ctx context.Context, imsis []string) (provisioner.RadiusBatch, error)
}

// Ops は手順に使うものをまとめたもの。
type Ops struct {
	DB  DB
	PV  Provisioner
	Log *slog.Logger
}

// Step は台帳を書く 1 つの操作。
type Step struct {
	// Check は台帳の今の行で操作できるかを確かめる（ロックの中）。できなければ断る文を返す。nil なら確かめない。
	Check func(e db.Entry, exists bool) string
	// Call は aka-provisioner を呼ぶ。nil なら呼ばない（台帳だけを書く）。
	Call func(ctx context.Context, e db.Entry) error
	// Change は成功したときの台帳の変更。nil なら台帳を書かない（認可ポリシーのルールなど、台帳にないもの）。
	Change func(e db.Entry, exists bool) db.Change
	// Tenant は、台帳にない IMSI の結果が分からないときに作る行のテナント（登録で選んだテナント）。
	Tenant int64
}

// Outcome は Step の結果。
type Outcome struct {
	// Refused は Check が断った文（呼ばなかった）。
	Refused string
	// Err は aka-provisioner の誤り（nil は成功）。
	Err error
	// Unknown は結果が分からなかったこと。Fixed は、その場で読み直して台帳を直したこと（Unknown のときだけ）。
	Unknown, Fixed bool
	// Succeeded は aka-provisioner が成功を返したこと（台帳を書けなかったときに、返す誤りと合わせて見る）。
	Succeeded bool
	// Entry と Exists は操作の後の台帳の行。
	Entry  db.Entry
	Exists bool
}

// Run は 1 つの操作を行う。返す誤りは DB の誤り（操作の結果は Outcome）。ctx には操作者とトレースID を入れておく。
// 下流の成功を書き損じないよう、呼び出し側は取り消されないコンテキストを渡す。
func (o *Ops) Run(ctx context.Context, meta db.Meta, imsi string, st Step) (Outcome, error) {
	var out Outcome
	var tenant int64
	err := o.DB.WithSubscriber(ctx, imsi, meta, func(l db.Ledger) error {
		e, ok := l.Entry()
		tenant = cmp.Or(st.Tenant, e.TenantID)
		if st.Check != nil {
			if msg := st.Check(e, ok); msg != "" {
				out.Refused, out.Entry, out.Exists = msg, e, ok
				return nil
			}
		}
		var callErr error
		if st.Call != nil {
			callErr = st.Call(ctx, e)
		}
		switch {
		case callErr == nil:
			out.Succeeded = true
			if st.Change != nil {
				if err := l.Apply(st.Change(e, ok)); err != nil {
					return err
				}
			}
		case provisioner.ResultUnknown(callErr) && st.Change != nil:
			out.Err, out.Unknown = callErr, true
			if err := l.Apply(db.Change{Kind: db.ChangeCheckNeeded, TenantID: tenant}); err != nil {
				return err
			}
			if v, rad, ok := o.Observe(ctx, imsi); ok {
				if err := l.Apply(db.Change{Kind: db.ChangeObserve, TenantID: tenant, Vecgen: v, Radius: rad, Reason: ReasonUnknown}); err != nil {
					return err
				}
				out.Fixed = true
			}
		default:
			out.Err, out.Unknown = callErr, provisioner.ResultUnknown(callErr)
		}
		out.Entry, out.Exists = l.Entry()
		return nil
	})
	if err != nil {
		o.Log.Error("ledger write failed", "event_id", "LEDGER_WRITE_ERR", "imsi", imsi, "downstream_succeeded", out.Succeeded,
			"trace_id", meta.TraceID, "error", err)
		if out.Succeeded && st.Change != nil && !errors.Is(err, db.ErrLedgerConflict) {
			// 下流は書き換えたのに台帳を書けなかった。別のトランザクションで印を立て、後で確かめる。
			o.markCheckNeeded(ctx, meta, imsi, tenant)
		}
	}
	return out, err
}

// markTimeout は、印を立てる DB の操作の上限時間。
const markTimeout = 5 * time.Second

// markCheckNeeded は IMSI に結果を確かめる印を立てる（失敗はログに出すだけ）。
func (o *Ops) markCheckNeeded(ctx context.Context, meta db.Meta, imsi string, tenant int64) {
	ctx, cancel := context.WithTimeout(ctx, markTimeout)
	defer cancel()
	err := o.DB.WithSubscriber(ctx, imsi, meta, func(l db.Ledger) error {
		return l.Apply(db.Change{Kind: db.ChangeCheckNeeded, TenantID: tenant})
	})
	if err != nil {
		o.Log.Error("cannot mark check_needed", "event_id", "LEDGER_WRITE_ERR", "imsi", imsi, "error", err)
	}
}

// observeTimeout は、結果を確かめるための読み直しの上限時間。
const observeTimeout = 10 * time.Second

// Observe は IMSI の両側を一括取得で読み直す。どちらかを読めなければ false。
func (o *Ops) Observe(ctx context.Context, imsi string) (v, rad db.Observation, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()
	vb, err := o.PV.VecgenBatchGet(ctx, []string{imsi})
	if err != nil {
		o.Log.Warn("recheck: vecgen batch-get", "imsi", imsi, "error", err)
		return v, rad, false
	}
	rb, err := o.PV.RadiusBatchGet(ctx, []string{imsi})
	if err != nil {
		o.Log.Warn("recheck: radius batch-get", "imsi", imsi, "error", err)
		return v, rad, false
	}
	for _, s := range vb.Items {
		if s.IMSI == imsi {
			v = db.Observation{Present: true, State: s.State}
		}
	}
	for _, s := range rb.Items {
		if s.IMSI == imsi {
			rad = db.Observation{Present: true, State: s.State, SourceID: s.SourceID}
		}
	}
	return v, rad, true
}

// Recheck は、印の立っている IMSI を読み直して台帳を直す。直せたら true と、直す前の所属のテナントを返す。
func (o *Ops) Recheck(ctx context.Context, meta db.Meta, imsi string) (tenant int64, fixed bool) {
	err := o.DB.WithSubscriber(ctx, imsi, meta, func(l db.Ledger) error {
		e, ok := l.Entry()
		if !ok || !e.CheckNeeded {
			return nil
		}
		v, rad, ok := o.Observe(ctx, imsi)
		if !ok {
			return nil
		}
		if err := l.Apply(db.Change{Kind: db.ChangeObserve, Vecgen: v, Radius: rad, Reason: ReasonUnknown}); err != nil {
			return err
		}
		tenant, fixed = e.TenantID, true
		return nil
	})
	if err != nil {
		o.Log.Error("recheck failed", "event_id", "LEDGER_WRITE_ERR", "imsi", imsi, "error", err)
		return 0, false
	}
	return tenant, fixed
}
