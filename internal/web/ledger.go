package web

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

// 加入者の操作の共通の手順（設計概要 §7.2・§8。段階 3-1 の W6・W7）。
// IMSI ごとのロックを取ったトランザクションの中で、台帳を確かめ、aka-provisioner を 1 回呼び、成功したら台帳と状態の事実を書く。
// 結果が分からない（503・502・タイムアウトなど）ときは、台帳を変えずに印（check_needed）を立て、その場で両側を一括取得で
// 読み直して台帳を直す（補いの事実 reconciled を書く）。読み直せなければ印を残し、画面に「結果を確かめています」と出す。

// reasonUnknown は、結果が分からなかった操作を読み直して台帳を直したときの、補いの事実の理由。
const reasonUnknown = "結果が分からなかった操作の確かめ"

// ledgerStep は台帳を書く 1 つの操作。
type ledgerStep struct {
	// Check は台帳の今の行で操作できるかを確かめる（ロックの中）。できなければ画面に出す文を返す。nil なら確かめない。
	Check func(e db.Entry, exists bool) string
	// Call は aka-provisioner を呼ぶ。nil なら呼ばない（台帳だけを書く）。
	Call func(ctx context.Context, e db.Entry) error
	// Change は成功したときの台帳の変更。nil なら台帳を書かない（認可ポリシーのルールなど、台帳にないもの）。
	Change func(e db.Entry, exists bool) db.Change
	// Tenant は、台帳にない IMSI の結果が分からないときに作る行のテナント（登録で選んだテナント）。
	Tenant int64
}

// ledgerOutcome は ledgerStep の結果。
type ledgerOutcome struct {
	// Refused は Check が断った文（呼ばなかった）。
	Refused string
	// Err は aka-provisioner の誤り（nil は成功）。
	Err error
	// Unknown は結果が分からなかったこと。Fixed は、その場で読み直して台帳を直したこと（Unknown のときだけ）。
	Unknown, Fixed bool
	// Entry と Exists は操作の後の台帳の行。
	Entry  db.Entry
	Exists bool
}

// meta は、利用者の操作の主体（状態の事実に書く）。
func meta(r *http.Request) db.Meta {
	return db.Meta{Operator: userFrom(r.Context()).Email, TraceID: trace.From(r.Context())}
}

// runLedger は 1 つの操作を行う。返す誤りは DB の誤り（操作の結果は ledgerOutcome）。
// 要求の取り消しで下流の成功を書き損じないよう、取り消されないコンテキストで行う。
func (h *Handler) runLedger(r *http.Request, imsi string, st ledgerStep) (ledgerOutcome, error) {
	ctx := context.WithoutCancel(r.Context())
	var out ledgerOutcome
	var tenant int64
	succeeded := false
	err := h.db.WithSubscriber(ctx, imsi, meta(r), func(l db.Ledger) error {
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
			succeeded = true
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
			if v, rad, ok := h.observe(ctx, imsi); ok {
				if err := l.Apply(db.Change{Kind: db.ChangeObserve, TenantID: tenant, Vecgen: v, Radius: rad, Reason: reasonUnknown}); err != nil {
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
		h.log.Error("ledger write failed", "event_id", "LEDGER_WRITE_ERR", "imsi", imsi, "downstream_succeeded", succeeded,
			"trace_id", trace.From(r.Context()), "error", err)
		if succeeded && st.Change != nil && !errors.Is(err, db.ErrLedgerConflict) {
			// 下流は書き換えたのに台帳を書けなかった。別のトランザクションで印を立て、後で確かめる。
			h.markCheckNeeded(ctx, r, imsi, tenant)
		}
	}
	return out, err
}

// markCheckNeeded は IMSI に結果を確かめる印を立てる（失敗はログに出すだけ）。
func (h *Handler) markCheckNeeded(ctx context.Context, r *http.Request, imsi string, tenant int64) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := h.db.WithSubscriber(ctx, imsi, meta(r), func(l db.Ledger) error {
		return l.Apply(db.Change{Kind: db.ChangeCheckNeeded, TenantID: tenant})
	})
	if err != nil {
		h.log.Error("cannot mark check_needed", "event_id", "LEDGER_WRITE_ERR", "imsi", imsi, "error", err)
	}
}

// observeTimeout は、結果を確かめるための読み直しの上限時間。
const observeTimeout = 10 * time.Second

// observe は IMSI の両側を一括取得で読み直す。どちらかを読めなければ false。
func (h *Handler) observe(ctx context.Context, imsi string) (v, rad db.Observation, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()
	vb, err := h.pv.VecgenBatchGet(ctx, []string{imsi})
	if err != nil {
		h.log.Warn("recheck: vecgen batch-get", "imsi", imsi, "error", err)
		return v, rad, false
	}
	rb, err := h.pv.RadiusBatchGet(ctx, []string{imsi})
	if err != nil {
		h.log.Warn("recheck: radius batch-get", "imsi", imsi, "error", err)
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

// recheck は、印の立っている IMSI を読み直して台帳を直す（詳細を開いたとき）。直せたら true。
func (h *Handler) recheck(r *http.Request, imsi string) bool {
	fixed := false
	ctx := context.WithoutCancel(r.Context())
	err := h.db.WithSubscriber(ctx, imsi, meta(r), func(l db.Ledger) error {
		e, ok := l.Entry()
		if !ok || !e.CheckNeeded {
			return nil
		}
		v, rad, ok := h.observe(ctx, imsi)
		if !ok {
			return nil
		}
		fixed = true
		return l.Apply(db.Change{Kind: db.ChangeObserve, Vecgen: v, Radius: rad, Reason: reasonUnknown})
	})
	if err != nil {
		h.log.Error("recheck failed", "event_id", "LEDGER_WRITE_ERR", "imsi", imsi, "error", err)
		return false
	}
	return fixed
}

// unknownMessage は、結果が分からなかったときの文（設計概要 §15 の「結果を確かめています」）。
func unknownMessage(what string, o ledgerOutcome) string {
	if o.Fixed {
		return what + "の結果が分かりませんでした（aka-provisioner か下流が応答しませんでした）。読み直して台帳を下流に合わせました。今の状態を確かめてください。"
	}
	return what + "の結果が分かりませんでした（aka-provisioner か下流が応答しませんでした）。結果を確かめています。しばらくしてから開き直してください。"
}
