package web

import (
	"context"
	"net/http"

	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/ledger"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

// 画面の操作から加入者の操作の手順（internal/ledger）を呼ぶ。

// meta は、利用者の操作の主体（状態の事実に書く）。
func meta(r *http.Request) db.Meta {
	return db.Meta{Operator: userFrom(r.Context()).Email, TraceID: trace.From(r.Context())}
}

// runLedger は 1 つの操作を行う。返す誤りは DB の誤り（操作の結果は ledger.Outcome）。
// 要求の取り消しで下流の成功を書き損じないよう、取り消されないコンテキストで行う。
func (h *Handler) runLedger(r *http.Request, imsi string, st ledger.Step) (ledger.Outcome, error) {
	return h.ops.Run(context.WithoutCancel(r.Context()), meta(r), imsi, st)
}

// recheck は、印の立っている IMSI を読み直して台帳を直す（詳細を開いたとき）。直せたら true。
func (h *Handler) recheck(r *http.Request, imsi string) bool {
	return h.ops.Recheck(context.WithoutCancel(r.Context()), meta(r), imsi)
}

// unknownMessage は、結果が分からなかったときの文（設計概要 §15 の「結果を確かめています」）。
func unknownMessage(what string, o ledger.Outcome) string {
	if o.Fixed {
		return what + "の結果が分かりませんでした（aka-provisioner か下流が応答しませんでした）。読み直して台帳を下流に合わせました。今の状態を確かめてください。"
	}
	return what + "の結果が分かりませんでした（aka-provisioner か下流が応答しませんでした）。結果を確かめています。しばらくしてから開き直してください。"
}
