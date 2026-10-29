package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// eventsText は状態の事実を比べやすい 1 行ずつの文にする。
func eventsText(evs []ChargingEvent) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %d %s→%s %s", ev.Event, ev.Side, ev.TenantID, ev.From, ev.To, ev.Reason)))
	}
	return out
}

func TestTransition(t *testing.T) {
	const imsi = "001010000000001"
	meta := Meta{Operator: "d@example.com", TraceID: "t1", JobID: "j1"}
	t0 := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	cur, exists := Entry{IMSI: imsi}, false
	step := 0
	apply := func(ch Change) Result {
		t.Helper()
		step++
		res, err := Transition(cur, exists, ch, meta, t0.Add(time.Duration(step)*time.Minute))
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if !res.Remove {
			cur, exists = res.Entry, true
		} else {
			cur, exists = Entry{IMSI: imsi}, false
		}
		return res
	}
	fails := func(ch Change) {
		t.Helper()
		if _, err := Transition(cur, exists, ch, meta, t0); !errors.Is(err, ErrLedgerConflict) {
			t.Errorf("%+v: err = %v, want conflict", ch, err)
		}
	}
	wantEvents := func(res Result, want ...string) {
		t.Helper()
		if got := eventsText(res.Events); !slices.Equal(got, want) {
			t.Errorf("events = %q, want %q", got, want)
		}
	}

	// 台帳にない IMSI の登録にはテナントが要る。状態の操作・削除はできない。
	fails(Change{Kind: ChangeRegister, Side: SideVecgen, State: StateActive})
	fails(Change{Kind: ChangeState, Side: SideVecgen, State: StateInactive})
	fails(Change{Kind: ChangeDelete, Side: SideRadius})

	res := apply(Change{Kind: ChangeRegister, Side: SideVecgen, TenantID: 10, State: StateActive})
	wantEvents(res, "line_registered vecgen 10 →active")
	if e := res.Entry; e.TenantID != 10 || !e.VecgenPresent || e.VecgenState != StateActive || e.RadiusPresent ||
		e.RegisteredBy != "d@example.com" || !e.RegisteredAt.Equal(t0.Add(time.Minute)) || !e.Mismatch() {
		t.Errorf("after register = %+v", e)
	}
	if ev := res.Events[0]; ev.IMSI != imsi || ev.Operator != "d@example.com" || ev.TraceID != "t1" || ev.JobID != "j1" ||
		!ev.Time.Equal(t0.Add(time.Minute)) {
		t.Errorf("event = %+v", ev)
	}
	// 同じ側の登録し直し、別のテナントでのもう一方の側の登録はできない。
	fails(Change{Kind: ChangeRegister, Side: SideVecgen, TenantID: 10, State: StateActive})
	fails(Change{Kind: ChangeRegister, Side: SideRadius, TenantID: 20, State: StateActive, SourceID: "vecgen"})
	fails(Change{Kind: ChangeRegister, Side: SideRadius, State: StateActive}) // 取得先が要る
	res = apply(Change{Kind: ChangeRegister, Side: SideRadius, State: StateInactive, SourceID: "vecgen"})
	wantEvents(res, "line_registered radius 10 →inactive")
	if e := res.Entry; !e.RadiusPresent || e.RadiusSourceID != "vecgen" || !e.RegisteredAt.Equal(t0.Add(time.Minute)) || !e.Mismatch() {
		t.Errorf("after radius = %+v", e)
	}

	res = apply(Change{Kind: ChangeState, Side: SideRadius, State: StateActive, Reason: "開通"})
	wantEvents(res, "line_state_changed radius 10 inactive→active 開通")
	if cur.Mismatch() {
		t.Errorf("mismatch after both active: %+v", cur)
	}
	// 同じ状態への変更は事実を書かず、行も変えない。
	if res = apply(Change{Kind: ChangeState, Side: SideRadius, State: StateActive}); len(res.Events) != 0 || res.Changed {
		t.Errorf("same state = %+v", res)
	}
	if res = apply(Change{Kind: ChangeSource, SourceID: "vecgen-2"}); len(res.Events) != 0 || cur.RadiusSourceID != "vecgen-2" || !res.Changed {
		t.Errorf("source = %+v", res)
	}
	res = apply(Change{Kind: ChangeMove, TenantID: 20})
	wantEvents(res, "line_tenant_moved  20 10→20")

	// 片側を消しても削除済みにはしない。両側を消したら削除済み。
	res = apply(Change{Kind: ChangeDelete, Side: SideVecgen})
	wantEvents(res, "line_deleted vecgen 20 active→")
	if cur.Deleted() || cur.VecgenPresent || cur.VecgenState != "" {
		t.Errorf("after vecgen delete = %+v", cur)
	}
	fails(Change{Kind: ChangeDelete, Side: SideVecgen})
	apply(Change{Kind: ChangeDelete, Side: SideRadius})
	if !cur.Deleted() || cur.RadiusSourceID != "" || cur.TenantID != 20 {
		t.Errorf("after both deleted = %+v", cur)
	}
	fails(Change{Kind: ChangeMove, TenantID: 10})

	// 削除済みの IMSI は、選んだテナントで新しく登録を始める（最初の登録の日時と操作者を改める）。
	fails(Change{Kind: ChangeRegister, Side: SideRadius, State: StateActive, SourceID: "vecgen"})
	res = apply(Change{Kind: ChangeRegister, Side: SideRadius, TenantID: 30, State: StateActive, SourceID: "vecgen"})
	if e := res.Entry; e.Deleted() || e.TenantID != 30 || !e.RegisteredAt.Equal(t0.Add(time.Duration(step)*time.Minute)) {
		t.Errorf("re-register = %+v", e)
	}
	wantEvents(res, "line_registered radius 30 →active")
}

func TestTransitionCheckAndObserve(t *testing.T) {
	const imsi = "001010000000002"
	now := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	meta := Meta{Operator: "c@example.com"}
	// 台帳にない IMSI の登録の結果が分からない: 選んだテナントで、印を立てた行を作る。
	res, err := Transition(Entry{IMSI: imsi}, false, Change{Kind: ChangeCheckNeeded, TenantID: 10}, meta, now)
	if err != nil || !res.Entry.CheckNeeded || !res.Entry.Pending() || res.Entry.TenantID != 10 || len(res.Events) != 0 {
		t.Fatalf("check needed = %+v, %v", res, err)
	}
	pending := res.Entry
	// 確かめたら、どちらの側にもなかった: 行を消す（削除済みにはしない）。
	res, err = Transition(pending, true, Change{Kind: ChangeObserve, Reason: "r"}, meta, now)
	if err != nil || !res.Remove || len(res.Events) != 0 {
		t.Errorf("observe nothing = %+v, %v", res, err)
	}
	// 確かめたら、vecgen 側にあった: 補いの事実を書き、印を下ろす。
	res, err = Transition(pending, true, Change{Kind: ChangeObserve, Reason: "結果の確かめ",
		Vecgen: Observation{Present: true, State: StateInactive}}, meta, now)
	if err != nil || res.Remove || res.Entry.CheckNeeded || !res.Entry.VecgenPresent || res.Entry.VecgenState != StateInactive {
		t.Fatalf("observe vecgen = %+v, %v", res, err)
	}
	if got := eventsText(res.Events); !slices.Equal(got, []string{"reconciled vecgen 10 →inactive 結果の確かめ"}) {
		t.Errorf("events = %q", got)
	}

	// 台帳と下流が違えば、台帳を下流に合わせる（状態・有無・取得先）。
	cur := Entry{IMSI: imsi, TenantID: 10, VecgenPresent: true, VecgenState: StateActive, RadiusPresent: true,
		RadiusState: StateActive, RadiusSourceID: "a", CheckNeeded: true}
	res, err = Transition(cur, true, Change{Kind: ChangeObserve, Vecgen: Observation{Present: true, State: StateActive},
		Radius: Observation{Present: true, State: StateInactive, SourceID: "b"}}, meta, now)
	if err != nil || res.Entry.RadiusState != StateInactive || res.Entry.RadiusSourceID != "b" || res.Entry.CheckNeeded {
		t.Errorf("observe diff = %+v, %v", res, err)
	}
	if got := eventsText(res.Events); !slices.Equal(got, []string{"reconciled radius 10 active→inactive"}) {
		t.Errorf("events = %q", got)
	}
	// 両側ともなくなっていた（下流で消された）: 削除済みにする。
	res, err = Transition(cur, true, Change{Kind: ChangeObserve}, meta, now)
	if err != nil || res.Remove || !res.Entry.Deleted() || len(res.Events) != 2 {
		t.Errorf("observe both gone = %+v, %v", res, err)
	}
	// 同じなら書かない。
	cur.CheckNeeded = false
	res, err = Transition(cur, true, Change{Kind: ChangeObserve, Vecgen: Observation{Present: true, State: StateActive},
		Radius: Observation{Present: true, State: StateActive, SourceID: "a"}}, meta, now)
	if err != nil || res.Changed || len(res.Events) != 0 {
		t.Errorf("observe same = %+v, %v", res, err)
	}
	// 印は 1 度だけ立てる。
	cur.CheckNeeded = true
	if res, err := Transition(cur, true, Change{Kind: ChangeCheckNeeded}, meta, now); err != nil || res.Changed {
		t.Errorf("check needed again = %+v, %v", res, err)
	}
}

func TestLedgerDB(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ta, _ := d.CreateTenant(ctx, "テナントA", Meta{})
	tb, _ := d.CreateTenant(ctx, "テナントB", Meta{})
	meta := Meta{Operator: "d@example.com", TraceID: "trace-1"}
	with := func(imsi string, fn func(Ledger) error) error { return d.WithSubscriber(ctx, imsi, meta, fn) }

	// 台帳と状態の事実を同じトランザクションで書く。
	if err := with("001010000000001", func(l Ledger) error {
		if _, ok := l.Entry(); ok {
			t.Error("entry exists before register")
		}
		if err := l.Apply(Change{Kind: ChangeRegister, Side: SideVecgen, TenantID: ta.ID, State: StateActive}); err != nil {
			return err
		}
		if e, ok := l.Entry(); !ok || !e.VecgenPresent {
			t.Errorf("entry after apply = %+v", e)
		}
		return l.Apply(Change{Kind: ChangeRegister, Side: SideRadius, State: StateActive, SourceID: "vecgen"})
	}); err != nil {
		t.Fatal(err)
	}
	e, err := d.Subscriber(ctx, "001010000000001")
	if err != nil || e.TenantID != ta.ID || e.VecgenState != StateActive || e.RadiusSourceID != "vecgen" || e.RegisteredBy != "d@example.com" ||
		e.RegisteredAt.IsZero() || e.Deleted() || e.CheckNeeded {
		t.Errorf("subscriber = %+v, %v", e, err)
	}
	evs, err := d.ChargingEvents(ctx, "001010000000001", 0, 10)
	if err != nil || len(evs) != 2 || evs[0].Event != EventLineRegistered || evs[0].Side != SideVecgen || evs[0].TraceID != "trace-1" ||
		evs[0].Operator != "d@example.com" || evs[1].Side != SideRadius || evs[0].ID >= evs[1].ID || evs[0].Time.IsZero() {
		t.Errorf("events = %+v, %v", evs, err)
	}

	// fn が誤りを返したら、台帳も状態の事実も書かない。
	boom := errors.New("boom")
	if err := with("001010000000002", func(l Ledger) error {
		if err := l.Apply(Change{Kind: ChangeRegister, Side: SideVecgen, TenantID: ta.ID, State: StateActive}); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if _, err := d.Subscriber(ctx, "001010000000002"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rolled back subscriber = %v", err)
	}
	if evs, _ := d.ChargingEvents(ctx, "001010000000002", 0, 10); len(evs) != 0 {
		t.Errorf("rolled back events = %+v", evs)
	}

	// 登録の結果を確かめている行を作り、確かめたらなかったので消す。
	if err := with("001010000000003", func(l Ledger) error {
		return l.Apply(Change{Kind: ChangeCheckNeeded, TenantID: tb.ID})
	}); err != nil {
		t.Fatal(err)
	}
	if e, err := d.Subscriber(ctx, "001010000000003"); err != nil || !e.CheckNeeded || !e.Pending() {
		t.Errorf("pending = %+v, %v", e, err)
	}
	if err := with("001010000000003", func(l Ledger) error { return l.Apply(Change{Kind: ChangeObserve}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Subscriber(ctx, "001010000000003"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removed pending = %v", err)
	}

	// 同じ IMSI の操作は順に行う（先の操作が終わるまで、後の操作は台帳を読めない）。
	hold, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- with("001010000000001", func(l Ledger) error {
			close(hold)
			<-release
			return l.Apply(Change{Kind: ChangeState, Side: SideVecgen, State: StateInactive})
		})
	}()
	<-hold
	second := make(chan Entry, 1)
	go func() {
		_ = d.WithSubscriber(context.WithoutCancel(ctx), "001010000000001", meta, func(l Ledger) error {
			e, _ := l.Entry()
			second <- e
			return nil
		})
	}()
	select {
	case e := <-second:
		t.Fatalf("second operation ran while the first held the lock: %+v", e)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e := <-second; e.VecgenState != StateInactive {
		t.Errorf("second operation read %+v, want the first's result", e)
	}

	// 一覧と件数の絞り込み。
	register(t, d, "001010000000010", ta.ID)
	register(t, d, "001010000000011", tb.ID)
	register(t, d, "001010000000012", ta.ID)
	if err := with("001010000000012", func(l Ledger) error { return l.Apply(Change{Kind: ChangeDelete, Side: SideVecgen}) }); err != nil {
		t.Fatal(err)
	}
	imsis := func(f SubscriberFilter, after string, limit int) string {
		t.Helper()
		l, err := d.ListSubscribers(ctx, f, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		n, err := d.CountSubscribers(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range l {
			out = append(out, e.IMSI[12:])
		}
		return fmt.Sprintf("%s n=%d", strings.Join(out, ","), n)
	}
	for _, c := range []struct {
		f     SubscriberFilter
		after string
		limit int
		want  string
	}{
		{SubscriberFilter{}, "", 10, "001,010,011 n=3"},
		{SubscriberFilter{}, "001010000000001", 1, "010 n=3"},
		{SubscriberFilter{TenantID: ta.ID}, "", 10, "001,010 n=2"},
		{SubscriberFilter{Prefix: "00101000000001"}, "", 10, "010,011 n=2"},
		{SubscriberFilter{VecgenState: StateInactive}, "", 10, "001 n=1"},
		{SubscriberFilter{RadiusState: FilterAbsent}, "", 10, "010,011 n=2"},
		{SubscriberFilter{SourceID: "vecgen"}, "", 10, "001 n=1"},
		{SubscriberFilter{Mismatch: true}, "", 10, "001,010,011 n=3"},
		{SubscriberFilter{Deleted: true}, "", 10, "012 n=1"},
		{SubscriberFilter{CheckNeeded: true}, "", 10, " n=0"},
	} {
		if got := imsis(c.f, c.after, c.limit); got != c.want {
			t.Errorf("list %+v after %q = %s, want %s", c.f, c.after, got, c.want)
		}
	}
	if src, err := d.SubscriberSources(ctx, 0); err != nil || !slices.Equal(src, []string{"vecgen"}) {
		t.Errorf("sources = %v, %v", src, err)
	}
	if src, _ := d.SubscriberSources(ctx, tb.ID); len(src) != 0 {
		t.Errorf("sources of tenant B = %v", src)
	}
	c, err := d.CountLedger(ctx, ta.ID)
	if err != nil || c.Total != 2 || c.Vecgen[StateActive] != 1 || c.Vecgen[StateInactive] != 1 || c.Radius[StateActive] != 1 ||
		c.Radius[FilterAbsent] != 1 || c.Mismatch != 2 || c.Deleted != 1 || c.CheckNeeded != 0 {
		t.Errorf("counts = %+v, %v", c, err)
	}
}
