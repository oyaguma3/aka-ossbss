package charging

import (
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

func jst(month, day, hour, minute int) time.Time {
	return time.Date(2026, time.Month(month), day, hour, minute, 0, 0, db.JST)
}

var november = NewPeriod(jst(11, 15, 0, 0))

const imsi = "001010000000001"

func reg(at time.Time, side db.Side, tenant int64, state string) db.ChargingEvent {
	return db.ChargingEvent{Time: at, Event: db.EventLineRegistered, Side: side, TenantID: tenant, IMSI: imsi, To: state}
}

func TestPeriodMask(t *testing.T) {
	p := november
	if p.Days() != 30 || p.Label() != "2026-11" || p.Compact() != "202611" || p.Prev().Label() != "2026-10" {
		t.Errorf("period = %d %s %s %s", p.Days(), p.Label(), p.Compact(), p.Prev().Label())
	}
	for _, c := range []struct {
		name   string
		t1, t2 time.Time
		want   uint32
	}{
		{"同じ日の中", jst(11, 2, 10, 0), jst(11, 2, 11, 0), 1 << 1},
		{"最後の 1 分で 1 日", jst(11, 30, 23, 59), jst(12, 1, 0, 0), 1 << 29},
		{"ちょうど 0 時の終わりは次の日に入らない", jst(11, 1, 0, 0), jst(11, 3, 0, 0), 0b11},
		{"0 時 1 分までなら次の日に入る", jst(11, 1, 0, 0), jst(11, 3, 0, 1), 0b111},
		{"長さがない", jst(11, 5, 1, 0), jst(11, 5, 1, 0), 0},
		{"月の外は切る", jst(10, 20, 0, 0), jst(11, 2, 12, 0), 0b11},
		{"月全体", jst(1, 1, 0, 0), jst(12, 31, 0, 0), 1<<30 - 1},
		{"月の外だけ", jst(10, 1, 0, 0), jst(11, 1, 0, 0), 0},
	} {
		if got := p.mask(c.t1, c.t2); got != c.want {
			t.Errorf("%s: mask = %b, want %b", c.name, got, c.want)
		}
	}
	// 31 日の月は最後の日のビットが立つ（符号つきの int4 に収まる）。
	oct := NewPeriod(jst(10, 1, 0, 0))
	if got := oct.mask(jst(10, 31, 23, 0), jst(11, 1, 0, 0)); got != 1<<30 {
		t.Errorf("31st = %b", got)
	}
	if got := p.MissingDays([]time.Time{jst(11, 2, 0, 0), jst(10, 31, 0, 0)}); got != (1<<30-1)&^(1<<1) {
		t.Errorf("missing = %b", got)
	}
}

func TestComputeLineWholeMonth(t *testing.T) {
	// 月初から Active のまま事実がない回線は、全部の日。
	res := ComputeLine(november, imsi, &db.OpenLine{IMSI: imsi, TenantID: 7, VecgenState: db.StateActive, RadiusState: db.StateActive}, nil)
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	r := res.Rows[0]
	if r.TenantID != 7 || r.RegisteredDays != 30 || r.RadiusActiveDays != 30 || r.VecgenActiveDays != 30 || !r.OpenEnd ||
		r.RadiusStart != db.StateActive || r.RadiusEnd != db.StateActive || !r.FirstRegisteredAt.IsZero() || !r.DeletedAt.IsZero() {
		t.Errorf("row = %+v", r)
	}
}

func TestComputeLineMonthEdges(t *testing.T) {
	// 課金レコード §6 の例: 月末の 23 時に Active にした回線は Active の日数が 1。
	res := ComputeLine(november, imsi, nil, []db.ChargingEvent{
		reg(jst(11, 30, 23, 0), db.SideRadius, 7, db.StateActive),
	})
	r := res.Rows[0]
	if r.RegisteredDays != 1 || r.RadiusActiveDays != 1 || r.VecgenActiveDays != 0 || r.RadiusStart != "" || r.RadiusEnd != db.StateActive ||
		!r.FirstRegisteredAt.Equal(jst(11, 30, 23, 0)) || !r.OpenEnd {
		t.Errorf("end of month = %+v", r)
	}
	// 月初の 0 時台に止めた回線は Active の日数が 1。
	res = ComputeLine(november, imsi, &db.OpenLine{IMSI: imsi, TenantID: 7, RadiusState: db.StateActive}, []db.ChargingEvent{
		{Time: jst(11, 1, 0, 30), Event: db.EventLineStateChanged, Side: db.SideRadius, TenantID: 7, IMSI: imsi, From: db.StateActive, To: db.StateInactive},
	})
	r = res.Rows[0]
	if r.RegisteredDays != 30 || r.RadiusActiveDays != 1 || r.RadiusStart != db.StateActive || r.RadiusEnd != db.StateInactive {
		t.Errorf("start of month = %+v", r)
	}
	// ちょうど 2 日 0 時に止めたら、1 日だけ。
	res = ComputeLine(november, imsi, &db.OpenLine{IMSI: imsi, TenantID: 7, RadiusState: db.StateActive}, []db.ChargingEvent{
		{Time: jst(11, 2, 0, 0), Event: db.EventLineStateChanged, Side: db.SideRadius, TenantID: 7, IMSI: imsi, To: db.StateInactive},
	})
	if r = res.Rows[0]; r.RadiusActiveDays != 1 {
		t.Errorf("at midnight = %+v", r)
	}
}

func TestComputeLineTenantMove(t *testing.T) {
	// 月の途中の付け替えは、テナントごとの行に分ける（決定事項 17）。付け替えの日は両方に数える。
	res := ComputeLine(november, imsi, &db.OpenLine{IMSI: imsi, TenantID: 7, VecgenState: db.StateActive, RadiusState: db.StateActive}, []db.ChargingEvent{
		{Time: jst(11, 11, 12, 0), Event: db.EventLineTenantMoved, TenantID: 8, IMSI: imsi, From: "7", To: "8"},
	})
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	a, b := res.Rows[0], res.Rows[1]
	if a.TenantID != 7 || a.RegisteredDays != 11 || a.RadiusActiveDays != 11 || a.RadiusStart != db.StateActive || a.RadiusEnd != db.StateActive || a.OpenEnd || !a.DeletedAt.IsZero() {
		t.Errorf("from = %+v", a)
	}
	if b.TenantID != 8 || b.RegisteredDays != 20 || b.RadiusActiveDays != 20 || b.RadiusStart != "" || b.RadiusEnd != db.StateActive || !b.OpenEnd || !b.FirstRegisteredAt.IsZero() {
		t.Errorf("to = %+v", b)
	}
	// テナントの数: 移動元は -1、移動先は +1。
	if len(res.Deltas) != 2 || res.Deltas[0].TenantID != 7 || res.Deltas[0].Lines != -1 || res.Deltas[0].RadiusActive != -1 ||
		res.Deltas[1].TenantID != 8 || res.Deltas[1].Lines != 1 || res.Deltas[1].RadiusActive != 1 {
		t.Errorf("deltas = %+v", res.Deltas)
	}
}

func TestComputeLineDeleteAndRegisterAgain(t *testing.T) {
	// 同じ月に削除して、別のテナントで登録し直す。
	res := ComputeLine(november, imsi, &db.OpenLine{IMSI: imsi, TenantID: 7, RadiusState: db.StateInactive}, []db.ChargingEvent{
		{Time: jst(11, 5, 9, 0), Event: db.EventLineDeleted, Side: db.SideRadius, TenantID: 7, IMSI: imsi, From: db.StateInactive},
		reg(jst(11, 10, 9, 0), db.SideRadius, 8, db.StateActive),
		reg(jst(11, 10, 9, 5), db.SideVecgen, 8, db.StateActive),
	})
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	a, b := res.Rows[0], res.Rows[1]
	if a.RegisteredDays != 5 || a.RadiusEnd != "" || !a.DeletedAt.Equal(jst(11, 5, 9, 0)) || a.OpenEnd {
		t.Errorf("deleted = %+v", a)
	}
	if b.RegisteredDays != 21 || b.RadiusActiveDays != 21 || b.VecgenActiveDays != 21 || !b.FirstRegisteredAt.Equal(jst(11, 10, 9, 0)) || !b.OpenEnd {
		t.Errorf("again = %+v", b)
	}
	// 同じテナントで登録し直すと 1 行で、日は続けて数えない（間の日は登録されていない）。
	res = ComputeLine(november, imsi, &db.OpenLine{IMSI: imsi, TenantID: 7, RadiusState: db.StateInactive}, []db.ChargingEvent{
		{Time: jst(11, 5, 9, 0), Event: db.EventLineDeleted, Side: db.SideRadius, TenantID: 7, IMSI: imsi},
		reg(jst(11, 10, 9, 0), db.SideRadius, 7, db.StateActive),
	})
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	r := res.Rows[0]
	if r.RegisteredDays != 26 || r.RadiusActiveDays != 21 || !r.DeletedAt.Equal(jst(11, 5, 9, 0)) || !r.FirstRegisteredAt.Equal(jst(11, 10, 9, 0)) || !r.OpenEnd || r.RadiusEnd != db.StateActive {
		t.Errorf("same tenant = %+v", r)
	}
}

func TestComputeLineReconciled(t *testing.T) {
	// 補いの事実は見つけた時刻の日に数える（Z4）。to が空ならその側がなくなる。
	res := ComputeLine(november, imsi, &db.OpenLine{IMSI: imsi, TenantID: 7, VecgenState: db.StateActive, RadiusState: db.StateActive}, []db.ChargingEvent{
		{Time: jst(11, 3, 4, 0), Event: db.EventReconciled, Side: db.SideRadius, TenantID: 7, IMSI: imsi, From: db.StateActive, To: db.StateInactive},
		{Time: jst(11, 4, 4, 0), Event: db.EventReconciled, Side: db.SideVecgen, TenantID: 7, IMSI: imsi, From: db.StateActive},
	})
	r := res.Rows[0]
	if r.RadiusActiveDays != 3 || r.VecgenActiveDays != 4 || r.VecgenEnd != "" || r.RadiusEnd != db.StateInactive || r.RegisteredDays != 30 || !r.OpenEnd {
		t.Errorf("reconciled = %+v", r)
	}
	// 時刻が前後しても番号の順に読む（前の時刻に合わせる）。
	res = ComputeLine(november, imsi, nil, []db.ChargingEvent{
		reg(jst(11, 5, 0, 10), db.SideRadius, 7, db.StateActive),
		{Time: jst(11, 5, 0, 9), Event: db.EventLineStateChanged, Side: db.SideRadius, TenantID: 7, IMSI: imsi, To: db.StateInactive},
	})
	if r = res.Rows[0]; r.RadiusEnd != db.StateInactive || r.RegisteredDays != 26 {
		t.Errorf("out of order = %+v", r)
	}
}

func TestAttachUsage(t *testing.T) {
	res := ComputeLine(november, imsi, nil, []db.ChargingEvent{reg(jst(11, 10, 0, 0), db.SideRadius, 7, db.StateActive)})
	missing := uint32(1 << 4) // 5 日は登録の前なので関係ない
	rows := AttachUsage(res.Rows, imsi, []db.UsageRow{
		{IMSI: imsi, TenantID: 0, Usage: db.Usage{AuthOK: 1}},
		{IMSI: imsi, TenantID: 7, Usage: db.Usage{AuthOK: 2, Sessions: 3, Seconds: 4, InOctets: 5, OutOctets: 6}},
		{IMSI: imsi, TenantID: 9, Usage: db.Usage{AuthOK: 7}},
	}, missing)
	if len(rows) != 3 || rows[0].TenantID != 0 || rows[1].TenantID != 7 || rows[2].TenantID != 9 {
		t.Fatalf("rows = %+v", rows)
	}
	// 刻印なしと、登録と違うテナントは registered_days が 0 の行のまま（Z5）。
	if rows[0].RegisteredDays != 0 || rows[0].AuthOK != 1 || rows[2].RegisteredDays != 0 || rows[2].AuthOK != 7 || rows[1].RegisteredDays != 21 ||
		rows[1].Usage != (db.Usage{AuthOK: 2, Sessions: 3, Seconds: 4, InOctets: 5, OutOctets: 6}) {
		t.Errorf("usage = %+v", rows)
	}
	if !rows[0].UsageComplete || !rows[1].UsageComplete || !rows[2].UsageComplete {
		t.Errorf("complete = %+v", rows)
	}
	// 登録されていた日に取り込めなかった日があれば false（Z10）。
	rows = AttachUsage(res.Rows, imsi, nil, 1<<9)
	if rows[0].UsageComplete {
		t.Errorf("incomplete = %+v", rows[0])
	}
}

func TestTenantCounter(t *testing.T) {
	c := NewTenantCounter()
	a := ComputeLine(november, imsi, &db.OpenLine{IMSI: imsi, TenantID: 7, RadiusState: db.StateActive}, []db.ChargingEvent{
		{Time: jst(11, 10, 0, 0), Event: db.EventLineTenantMoved, TenantID: 8, IMSI: imsi, To: "8"},
	})
	c.Add(a)
	c.Add(ComputeLine(november, "001010000000002", &db.OpenLine{IMSI: "001010000000002", TenantID: 7, RadiusState: db.StateInactive}, nil))
	c.Add(ComputeLine(november, "001010000000003", nil, []db.ChargingEvent{
		reg(jst(11, 5, 0, 0), db.SideRadius, 7, db.StateActive),
		{Time: jst(11, 6, 0, 0), Event: db.EventLineDeleted, Side: db.SideRadius, TenantID: 7},
	}))
	tenant7 := db.ChargingTenant{TenantID: 7}
	c.Fill(&tenant7)
	// 月初 2（Active 1）→ 5 日に 3（Active 2）→ 6 日に 2（Active 1）→ 10 日に 1（Active 0）。
	if tenant7.LinesEnd != 1 || tenant7.LinesMax != 3 || tenant7.RadiusActiveLinesEnd != 0 || tenant7.RadiusActiveLinesMax != 2 {
		t.Errorf("tenant 7 = %+v", tenant7)
	}
	tenant8 := db.ChargingTenant{TenantID: 8}
	c.Fill(&tenant8)
	if tenant8.LinesEnd != 1 || tenant8.LinesMax != 1 || tenant8.RadiusActiveLinesEnd != 1 {
		t.Errorf("tenant 8 = %+v", tenant8)
	}
	// 同じ時刻の増減はまとめて足す（途中の値を最大に数えない）。
	c = NewTenantCounter()
	c.Add(LineResult{Deltas: []TenantDelta{{TenantID: 1, At: jst(11, 2, 0, 0), Lines: 1}, {TenantID: 1, At: jst(11, 2, 0, 0), Lines: -1}}})
	tenant1 := db.ChargingTenant{TenantID: 1}
	c.Fill(&tenant1)
	if tenant1.LinesMax != 0 {
		t.Errorf("same time = %+v", tenant1)
	}
}

func TestComputeTenant(t *testing.T) {
	// 月の途中で作って名前を変え、外部の顧客番号の変更では名前を変えない。
	got, ok := ComputeTenant(november, 7, nil, []db.ChargingEvent{
		{Time: jst(11, 10, 0, 0), Event: db.EventTenantCreated, TenantID: 7, To: "A"},
		{Time: jst(11, 11, 0, 0), Event: db.EventTenantUpdated, TenantID: 7, From: "A", To: "B"},
		{Time: jst(11, 12, 0, 0), Event: db.EventTenantUpdated, TenantID: 7, To: "C-001", Reason: db.TenantExternalReason},
	})
	if !ok || got.Name != "B" || got.ExistedDays != 21 || !got.CreatedAt.Equal(jst(11, 10, 0, 0)) || !got.DeletedAt.IsZero() || !got.LiveEnd {
		t.Errorf("created = %+v, %v", got, ok)
	}
	// 月の途中で削除した。
	got, ok = ComputeTenant(november, 7, &db.ChargingTenant{Name: "A", LiveEnd: true}, []db.ChargingEvent{
		{Time: jst(11, 3, 12, 0), Event: db.EventTenantDeleted, TenantID: 7, From: "A"},
	})
	if !ok || got.Name != "A" || got.ExistedDays != 3 || !got.DeletedAt.Equal(jst(11, 3, 12, 0)) || got.LiveEnd {
		t.Errorf("deleted = %+v, %v", got, ok)
	}
	// 事実のない月もテナントは出す。前の月に削除したテナントは出さない。
	if got, ok = ComputeTenant(november, 7, &db.ChargingTenant{Name: "A", LiveEnd: true}, nil); !ok || got.ExistedDays != 30 {
		t.Errorf("no facts = %+v, %v", got, ok)
	}
	if _, ok = ComputeTenant(november, 7, &db.ChargingTenant{Name: "A"}, nil); ok {
		t.Error("a deleted tenant must not appear")
	}
}
