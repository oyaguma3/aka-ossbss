// Package reconcile は台帳と下流の突き合わせ（設計概要 §9。段階 3-3）と、結果を確かめる印の定期の読み直し（Y10）を受け持つ。
//
// 下流が正本なので、台帳の側を直す。① 台帳と両側の一覧を IMSI の順に併合してずれの候補を出す → ② 候補を一括取得で読み直して絞る →
// ③ IMSI ごとのロックの中で読み直して直す（画面と一括投入は同じロックを持つので、同時の操作と重ならない）。一覧を読み終えるまで何も直さない。
// 直す数が上限を超えたら何も直さず保留にする（下流を古いバックアップから戻したときなどに、台帳と課金レコードを大量に書き換えないため）。
package reconcile

import (
	"slices"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// view は下流の 1 つの側の値。
type view struct {
	present bool
	state   string
	source  string
	// ruleTenants はルールのテナント（radius 側で、読んだときだけ）。
	ruleTenants []int64
}

func vecgenView(s provisioner.VecgenSubscriber) view { return view{present: true, state: s.State} }

func radiusView(s provisioner.RadiusSubscriber) view {
	v := view{present: true, state: s.State, source: s.SourceID}
	for _, r := range s.Rules {
		v.ruleTenants = append(v.ruleTenants, r.TenantID)
	}
	return v
}

func observationView(o db.Observation) view {
	return view{present: o.Present, state: o.State, source: o.SourceID}
}

// commonRuleTenant は、ルールのテナントが全部同じならそのテナントを返す（違えば 0）。
func commonRuleTenant(tenants []int64) int64 {
	if len(tenants) == 0 {
		return 0
	}
	for _, t := range tenants[1:] {
		if t != tenants[0] {
			return 0
		}
	}
	return tenants[0]
}

// Fixable は自動で直すずれか（Y5）。
func Fixable(kind string) bool {
	switch kind {
	case db.FindingLedgerOnly, db.FindingStateDiff, db.FindingSourceDiff, db.FindingSideAbsent:
		return true
	}
	return false
}

// compare は台帳の行と両側を比べてずれを返す（Y5）。Status は入れない。
func compare(e db.Entry, exists bool, v, r view, imsi string) []db.Finding {
	var out []db.Finding
	add := func(kind string, side db.Side, ledger, downstream string, tenant int64) {
		out = append(out, db.Finding{IMSI: imsi, Kind: kind, Side: side, LedgerVal: ledger, DownstreamVal: downstream, TenantID: tenant})
	}
	if !exists || e.Deleted() {
		// IMSI が台帳にない・台帳で削除済みなのに下流にある: システム管理者が決める。
		kind, ledger, tenant := db.FindingDownstreamOnly, "", commonRuleTenant(r.ruleTenants)
		if exists {
			kind, ledger = db.FindingDeletedPresent, "削除済み"
			if tenant == 0 {
				tenant = e.TenantID
			}
		}
		if v.present {
			add(kind, db.SideVecgen, ledger, v.state, tenant)
		}
		if r.present {
			add(kind, db.SideRadius, ledger, r.state, tenant)
		}
		return out
	}
	for _, s := range db.Sides {
		d := v
		if s == db.SideRadius {
			d = r
		}
		switch {
		case e.Present(s) && !d.present:
			add(db.FindingLedgerOnly, s, e.State(s), "", e.TenantID)
		case !e.Present(s) && d.present:
			add(db.FindingSideAbsent, s, "", d.state, e.TenantID)
		case !e.Present(s):
		case e.State(s) != d.state:
			add(db.FindingStateDiff, s, e.State(s), d.state, e.TenantID)
		}
	}
	if e.RadiusPresent && r.present && e.RadiusSourceID != r.source {
		add(db.FindingSourceDiff, db.SideRadius, e.RadiusSourceID, r.source, e.TenantID)
	}
	if r.present && slices.ContainsFunc(r.ruleTenants, func(t int64) bool { return t != e.TenantID }) {
		add(db.FindingRuleTenant, db.SideRadius, strconv.FormatInt(e.TenantID, 10), joinTenants(r.ruleTenants), e.TenantID)
	}
	return out
}

func joinTenants(ids []int64) string {
	sorted := slices.Sorted(slices.Values(ids))
	sorted = slices.Compact(sorted)
	parts := make([]string, 0, len(sorted))
	for _, id := range sorted {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ", ")
}
