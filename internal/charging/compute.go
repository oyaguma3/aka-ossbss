// Package charging は課金レコードの月次の計算、ファイルの生成、取り込みと締めの実行を受け持つ
// （aka-mvpset の docs/charging-records.md。段階 3-4 の Z1〜Z16）。
//
// 料金は計算しない。状態の事実（charging_events）と利用の集計（usage_daily）から、回線とテナントの月次の値を作り、
// CSV と manifest を ZIP にまとめて PGP で暗号化する。
package charging

import (
	"cmp"
	"math/bits"
	"slices"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// Period は対象月（日本時間のその月の 1 日 0 時から翌月 1 日 0 時まで）。
type Period struct {
	Start, End time.Time
}

// NewPeriod は month を含む月を返す。
func NewPeriod(month time.Time) Period {
	s := db.MonthOf(month)
	return Period{Start: s, End: s.AddDate(0, 1, 0)}
}

// Prev は前の月を返す。
func (p Period) Prev() Period { return NewPeriod(p.Start.AddDate(0, -1, 0)) }

// Label は YYYY-MM の形（CSV の period）、Compact は YYYYMM の形（record_id とファイルの名前）。
func (p Period) Label() string   { return p.Start.Format("2006-01") }
func (p Period) Compact() string { return p.Start.Format("200601") }

// Days はその月の日数。
func (p Period) Days() int { return p.End.AddDate(0, 0, -1).Day() }

// mask は区間 [t1, t2) が少しでもかかる日のビット（1 日が最下位）を返す。ちょうど 0 時に終わる区間は、次の日に入らない。
func (p Period) mask(t1, t2 time.Time) uint32 {
	if t1.Before(p.Start) {
		t1 = p.Start
	}
	if t2.After(p.End) {
		t2 = p.End
	}
	if !t1.Before(t2) {
		return 0
	}
	first, last := t1.In(db.JST).Day()-1, t2.Add(-time.Nanosecond).In(db.JST).Day()-1
	return uint32(1)<<(last+1) - uint32(1)<<first
}

// MissingDays は、その月の日のうち imported にない日のビットを返す。
func (p Period) MissingDays(imported []time.Time) uint32 {
	all := p.mask(p.Start, p.End)
	for _, d := range imported {
		if !d.Before(p.Start) && d.Before(p.End) {
			all &^= uint32(1) << (d.Day() - 1)
		}
	}
	return all
}

// lineState はある時点の回線の状態（どちらかの側があれば登録されている。ない側は空）。
type lineState struct {
	tenant         int64
	vecgen, radius string
}

func (s lineState) present() bool { return s.vecgen != "" || s.radius != "" }

// TenantDelta はテナントの回線の数の増減。
type TenantDelta struct {
	TenantID     int64
	At           time.Time
	Lines        int
	RadiusActive int
}

// LineResult は 1 つの IMSI の月次の計算の結果。
type LineResult struct {
	// Rows はテナントごとの行（テナントの順）。利用の項目は AttachUsage で足す。
	Rows []db.ChargingLine
	// Start は月初の状態（なければ nil）。Deltas は月の中の増減。
	Start  *db.OpenLine
	Deltas []TenantDelta
}

// lineMasks は 1 つの行の日のビット。
type lineMasks struct{ registered, radiusActive, vecgenActive uint32 }

// ComputeLine は、月初の状態（なければ nil。最初の月は事実だけで計算する。Z3）とその月の事実（番号の順）から、1 つの IMSI の行を作る。
// 行はテナントごとに分ける（決定事項 17）。どの事実も tenant_id がその時の所属なので、変わったら別の行に移る。
//
// 事実の読み方: line_registered・line_state_changed・reconciled は to がその側の新しい状態（reconciled の to が空ならその側がなくなった）、
// line_deleted はその側がなくなる、line_tenant_moved は所属だけが変わる。同じ IMSI の事実はロックの順に番号が付くが、時刻は
// トランザクションの始まりなので少し前後することがある。番号の順に読み、時刻が戻ったら前の時刻に合わせる。
func ComputeLine(p Period, imsi string, start *db.OpenLine, events []db.ChargingEvent) LineResult {
	res := LineResult{Start: start}
	rows := map[int64]*db.ChargingLine{}
	masks := map[int64]*lineMasks{}
	row := func(tenant int64) *db.ChargingLine {
		r, ok := rows[tenant]
		if !ok {
			r = &db.ChargingLine{IMSI: imsi, TenantID: tenant}
			rows[tenant], masks[tenant] = r, &lineMasks{}
		}
		return r
	}
	var cur lineState
	if start != nil {
		cur = lineState{tenant: start.TenantID, vecgen: start.VecgenState, radius: start.RadiusState}
		if cur.present() {
			r := row(cur.tenant)
			r.VecgenStart, r.RadiusStart = cur.vecgen, cur.radius
		}
	}
	at := p.Start
	flush := func(until time.Time) {
		if !cur.present() {
			return
		}
		row(cur.tenant)
		m := masks[cur.tenant]
		d := p.mask(at, until)
		m.registered |= d
		if cur.radius == db.StateActive {
			m.radiusActive |= d
		}
		if cur.vecgen == db.StateActive {
			m.vecgenActive |= d
		}
	}
	setEnd := func(s lineState) {
		if r, ok := rows[s.tenant]; ok {
			r.VecgenEnd, r.RadiusEnd = s.vecgen, s.radius
		}
	}
	for _, ev := range events {
		t := ev.Time
		if t.Before(at) {
			t = at
		}
		if t.After(p.End) {
			t = p.End
		}
		flush(t)
		at = t
		before := cur
		if ev.TenantID != cur.tenant {
			setEnd(cur)
			cur.tenant = ev.TenantID
		}
		switch ev.Event {
		case db.EventLineRegistered, db.EventLineStateChanged, db.EventReconciled:
			setSide(&cur, ev.Side, ev.To)
		case db.EventLineDeleted:
			setSide(&cur, ev.Side, "")
		}
		if cur.present() {
			r := row(cur.tenant)
			if !before.present() && ev.Event != db.EventLineTenantMoved && r.FirstRegisteredAt.IsZero() {
				r.FirstRegisteredAt = ev.Time
			}
		} else if before.present() {
			row(before.tenant).DeletedAt = ev.Time
		}
		res.Deltas = append(res.Deltas, stateDeltas(before, cur, ev.Time)...)
	}
	flush(p.End)
	setEnd(cur)
	if r, ok := rows[cur.tenant]; ok {
		r.OpenEnd = cur.present()
	}
	res.Rows = make([]db.ChargingLine, 0, len(rows))
	for tenant, r := range rows {
		m := masks[tenant]
		r.RegisteredMask = m.registered
		r.RegisteredDays = bits.OnesCount32(m.registered)
		r.RadiusActiveDays = bits.OnesCount32(m.radiusActive)
		r.VecgenActiveDays = bits.OnesCount32(m.vecgenActive)
		res.Rows = append(res.Rows, *r)
	}
	slices.SortFunc(res.Rows, func(a, b db.ChargingLine) int { return cmp.Compare(a.TenantID, b.TenantID) })
	return res
}

func setSide(s *lineState, side db.Side, state string) {
	if side == db.SideVecgen {
		s.vecgen = state
	} else if side == db.SideRadius {
		s.radius = state
	}
}

// stateDeltas は状態の変化をテナントの数の増減にする。
func stateDeltas(before, after lineState, at time.Time) []TenantDelta {
	var out []TenantDelta
	same := before.tenant == after.tenant
	add := func(tenant int64, lines, radius int) {
		if lines != 0 || radius != 0 {
			out = append(out, TenantDelta{TenantID: tenant, At: at, Lines: lines, RadiusActive: radius})
		}
	}
	b2i := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	bl, br := b2i(before.present()), b2i(before.radius == db.StateActive)
	al, ar := b2i(after.present()), b2i(after.radius == db.StateActive)
	if same {
		add(after.tenant, al-bl, ar-br)
		return out
	}
	add(before.tenant, -bl, -br)
	add(after.tenant, al, ar)
	return out
}

// AttachUsage は行に利用の集計を足す。利用の行は集計の刻印のテナントのままにし、台帳のテナントに付け替えない。登録と違うテナントなら
// registered_days が 0 の行、刻印なしは tenant_id 0 の行になる（Z5）。usage_complete は、その行の登録されていた日に
// 取り込めなかった日があれば false（Z10）。
func AttachUsage(rows []db.ChargingLine, imsi string, usage []db.UsageRow, missing uint32) []db.ChargingLine {
	for _, u := range usage {
		i := slices.IndexFunc(rows, func(r db.ChargingLine) bool { return r.TenantID == u.TenantID })
		if i < 0 {
			rows = append(rows, db.ChargingLine{IMSI: imsi, TenantID: u.TenantID})
			i = len(rows) - 1
		}
		rows[i].Usage = u.Usage
	}
	for i := range rows {
		rows[i].UsageComplete = rows[i].RegisteredMask&missing == 0
	}
	slices.SortFunc(rows, func(a, b db.ChargingLine) int { return cmp.Compare(a.TenantID, b.TenantID) })
	return rows
}

// TenantCounter は回線の増減をテナントごとに集める。
type TenantCounter struct {
	base   map[int64]*tenantBase
	deltas map[int64][]TenantDelta
}

type tenantBase struct{ lines, radiusActive int }

// NewTenantCounter は TenantCounter を作る。
func NewTenantCounter() *TenantCounter {
	return &TenantCounter{base: map[int64]*tenantBase{}, deltas: map[int64][]TenantDelta{}}
}

// Add は 1 つの IMSI の結果を足す。
func (c *TenantCounter) Add(r LineResult) {
	if s := r.Start; s != nil && (s.VecgenState != "" || s.RadiusState != "") {
		b, ok := c.base[s.TenantID]
		if !ok {
			b = &tenantBase{}
			c.base[s.TenantID] = b
		}
		b.lines++
		if s.RadiusState == db.StateActive {
			b.radiusActive++
		}
	}
	for _, d := range r.Deltas {
		c.deltas[d.TenantID] = append(c.deltas[d.TenantID], d)
	}
}

// Fill はテナントの月末と最大の回線の数を入れる。同じ時刻の増減はまとめて足す（同じトランザクションの途中の値を最大に数えない）。
func (c *TenantCounter) Fill(t *db.ChargingTenant) {
	var lines, radius int
	if b, ok := c.base[t.TenantID]; ok {
		lines, radius = b.lines, b.radiusActive
	}
	maxLines, maxRadius := lines, radius
	ds := c.deltas[t.TenantID]
	slices.SortStableFunc(ds, func(a, b TenantDelta) int { return a.At.Compare(b.At) })
	for i, d := range ds {
		lines += d.Lines
		radius += d.RadiusActive
		if i+1 == len(ds) || !ds[i+1].At.Equal(d.At) {
			maxLines, maxRadius = max(maxLines, lines), max(maxRadius, radius)
		}
	}
	t.LinesEnd, t.LinesMax, t.RadiusActiveLinesEnd, t.RadiusActiveLinesMax = lines, maxLines, radius, maxRadius
}

// ComputeTenant は、月初の値（なければ nil）とその月のテナントの事実（番号の順）から、テナントの月次の行を作る。その月に一度も
// 存在しなかったテナントは false を返す。名前は月末の値。
func ComputeTenant(p Period, id int64, start *db.ChargingTenant, events []db.ChargingEvent) (db.ChargingTenant, bool) {
	t := db.ChargingTenant{TenantID: id}
	live := false
	if start != nil {
		t.Name, live = start.Name, start.LiveEnd
	}
	var mask uint32
	at := p.Start
	for _, ev := range events {
		e := ev.Time
		if e.Before(at) {
			e = at
		}
		if live {
			mask |= p.mask(at, e)
		}
		at = e
		switch ev.Event {
		case db.EventTenantCreated:
			live, t.Name, t.CreatedAt = true, ev.To, ev.Time
		case db.EventTenantUpdated:
			if ev.Reason != db.TenantExternalReason {
				t.Name = ev.To
			}
		case db.EventTenantDeleted:
			live, t.DeletedAt = false, ev.Time
		}
	}
	if live {
		mask |= p.mask(at, p.End)
	}
	t.ExistedDays, t.LiveEnd = bits.OnesCount32(mask), live
	return t, live || mask != 0 || !t.CreatedAt.IsZero() || !t.DeletedAt.IsZero()
}
