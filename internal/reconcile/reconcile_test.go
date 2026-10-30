package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

var deletedAt = time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

// memStore はメモリーの DB。台帳は db.Transition で書く（本物の DB と同じ移り方）。
type memStore struct {
	mu       sync.Mutex
	subs     map[string]db.Entry
	events   []db.ChargingEvent
	runs     map[int64]*db.ReconcileRun
	findings []db.Finding
	audit    []db.AuditEntry
	tenants  []int64
	locked   bool
	// entriesErr を設定すると台帳の読み取りが誤りを返す。
	entriesErr error
	// onLock は IMSI のロックを取るときに呼ぶ（処理中だった操作がここで終わる場合の試験）。
	onLock func(imsi string)
}

func newMemStore() *memStore {
	return &memStore{subs: map[string]db.Entry{}, runs: map[int64]*db.ReconcileRun{}, tenants: []int64{10, 20}}
}

type memLedger struct {
	s      *memStore
	meta   db.Meta
	cur    db.Entry
	exists bool
	events []db.ChargingEvent
}

func (l *memLedger) Entry() (db.Entry, bool) { return l.cur, l.exists }
func (l *memLedger) Apply(ch db.Change) error {
	res, err := db.Transition(l.cur, l.exists, ch, l.meta, time.Now())
	if err != nil {
		return err
	}
	if e := res.Entry; res.Changed && !res.Remove && !e.Deleted() && !slices.Contains(l.s.tenants, e.TenantID) {
		return db.ErrTenantNotFound
	}
	l.cur, l.exists = res.Entry, !res.Remove
	l.events = append(l.events, res.Events...)
	return nil
}

func (s *memStore) WithSubscriber(_ context.Context, imsi string, meta db.Meta, fn func(db.Ledger) error) error {
	if s.onLock != nil {
		s.onLock(imsi)
	}
	s.mu.Lock()
	cur, ok := s.subs[imsi]
	s.mu.Unlock()
	if !ok {
		cur = db.Entry{IMSI: imsi}
	}
	l := &memLedger{s: s, meta: meta, cur: cur, exists: ok}
	if err := fn(l); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.exists {
		s.subs[imsi] = l.cur
	} else {
		delete(s.subs, imsi)
	}
	s.events = append(s.events, l.events...)
	return nil
}

func (s *memStore) InsertAudit(_ context.Context, e db.AuditEntry) (int64, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, e)
	return int64(len(s.audit)), time.Now(), nil
}

func (s *memStore) ReconcileEntries(_ context.Context, after string, limit int) ([]db.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entriesErr != nil {
		return nil, s.entriesErr
	}
	var out []db.Entry
	for _, imsi := range slices.Sorted(maps.Keys(s.subs)) {
		if imsi > after && len(out) < limit {
			out = append(out, s.subs[imsi])
		}
	}
	return out, nil
}

func (s *memStore) SubscribersByIMSI(_ context.Context, imsis []string) (map[string]db.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]db.Entry{}
	for _, imsi := range imsis {
		if e, ok := s.subs[imsi]; ok {
			out[imsi] = e
		}
	}
	return out, nil
}

func (s *memStore) CreateReconcileRun(_ context.Context, trigger, by, reason string) (db.ReconcileRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &db.ReconcileRun{ID: int64(len(s.runs) + 1), Trigger: trigger, RequestedBy: by, ApproveReason: reason, State: db.RunRunning}
	s.runs[r.ID] = r
	return *r, nil
}

func (s *memStore) UpdateReconcileProgress(context.Context, int64, int, int, int) error { return nil }

func (s *memStore) FinishReconcileRun(_ context.Context, r db.ReconcileRun) (db.ReconcileRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.runs[r.ID] = r
	return r, nil
}

func (s *memStore) InterruptRunningReconciles(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.State == db.RunRunning {
			r.State = db.RunInterrupted
		}
	}
	return nil
}

func (s *memStore) AddFindings(_ context.Context, runID int64, fs []db.Finding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range fs {
		f.RunID = runID
		f.ID = int64(len(s.findings) + 1)
		s.findings = append(s.findings, f)
	}
	return nil
}

func (s *memStore) ResolveFindings(_ context.Context, runID int64, imsi string, side db.Side, status, by string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for i := range s.findings {
		f := &s.findings[i]
		if f.RunID == runID && f.IMSI == imsi && (side == "" || f.Side == side) && f.Status == db.FindingPending {
			f.Status, f.ResolvedBy = status, by
			n++
		}
	}
	return n, nil
}

func (s *memStore) CheckNeededIMSIs(_ context.Context, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, imsi := range slices.Sorted(maps.Keys(s.subs)) {
		if s.subs[imsi].CheckNeeded && len(out) < limit {
			out = append(out, imsi)
		}
	}
	return out, nil
}

func (s *memStore) TryNamedLock(context.Context, string) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return nil, false, nil
	}
	s.locked = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.locked = false
	}, true, nil
}

// findingTexts はずれを「種類 側 状態」で返す。
func (s *memStore) findingTexts(imsi string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, f := range s.findings {
		if f.IMSI == imsi {
			out = append(out, f.Kind+" "+string(f.Side)+" "+f.Status)
		}
	}
	slices.Sort(out)
	return out
}

// memPV はメモリーの aka-provisioner。
type memPV struct {
	mu     sync.Mutex
	vecgen map[string]string
	radius map[string]provisioner.RadiusSubscriber
	// listStale を設定すると、一覧はこの時点の写しを返す（読み取りの間に下流が変わった場合）。
	vecgenStale map[string]string
	writes      []string
	// listErrAfter は、一覧を何回読んだ後に誤りを返すか（0 は返さない）。
	listErrAfter, listCalls int
	batchErr                error
	writeErr                error
	// onList は一覧を読むたびに呼ぶ（試験で実行の途中に手を入れる）。
	onList func()
}

func newMemPV() *memPV {
	return &memPV{vecgen: map[string]string{}, radius: map[string]provisioner.RadiusSubscriber{}}
}

func (p *memPV) addRadius(imsi, state, source string, tenants ...int64) {
	s := provisioner.RadiusSubscriber{IMSI: imsi, State: state, SourceID: source}
	for _, t := range tenants {
		s.Rules = append(s.Rules, provisioner.Rule{TenantID: t, NASID: "*", SSIDs: []string{"*"}})
	}
	p.radius[imsi] = s
}

func page[T any](keys []string, cursor string, limit int, get func(string) T) ([]T, string) {
	slices.Sort(keys)
	var out []T
	last := ""
	for _, k := range keys {
		if k <= cursor {
			continue
		}
		if len(out) == limit {
			return out, last
		}
		out = append(out, get(k))
		last = k
	}
	return out, ""
}

func (p *memPV) tickList() error {
	p.mu.Lock()
	p.listCalls++
	failed := p.listErrAfter > 0 && p.listCalls > p.listErrAfter
	on := p.onList
	p.mu.Unlock()
	if on != nil {
		on()
	}
	if failed {
		return &provisioner.Error{Status: 503, Problem: provisioner.Problem{Status: 503, Cause: provisioner.CauseDownstreamUnavailable}}
	}
	return nil
}

func (p *memPV) ListVecgenSubscribers(_ context.Context, cursor string, limit int) (provisioner.VecgenList, error) {
	if err := p.tickList(); err != nil {
		return provisioner.VecgenList{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.vecgen
	if p.vecgenStale != nil {
		src = p.vecgenStale
	}
	items, next := page(slices.Collect(maps.Keys(src)), cursor, limit, func(k string) provisioner.VecgenSubscriber {
		return provisioner.VecgenSubscriber{IMSI: k, State: src[k]}
	})
	return provisioner.VecgenList{Items: items, NextCursor: next}, nil
}

func (p *memPV) ListRadiusSubscribers(_ context.Context, cursor string, limit int) (provisioner.RadiusList, error) {
	if err := p.tickList(); err != nil {
		return provisioner.RadiusList{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	items, next := page(slices.Collect(maps.Keys(p.radius)), cursor, limit, func(k string) provisioner.RadiusSubscriber { return p.radius[k] })
	return provisioner.RadiusList{Items: items, NextCursor: next}, nil
}

func (p *memPV) VecgenBatchGet(_ context.Context, imsis []string) (provisioner.VecgenBatch, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b provisioner.VecgenBatch
	if p.batchErr != nil {
		return b, p.batchErr
	}
	for _, imsi := range imsis {
		if st, ok := p.vecgen[imsi]; ok {
			b.Items = append(b.Items, provisioner.VecgenSubscriber{IMSI: imsi, State: st})
		}
	}
	return b, nil
}

func (p *memPV) RadiusBatchGet(_ context.Context, imsis []string) (provisioner.RadiusBatch, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b provisioner.RadiusBatch
	if p.batchErr != nil {
		return b, p.batchErr
	}
	for _, imsi := range imsis {
		if s, ok := p.radius[imsi]; ok {
			b.Items = append(b.Items, s)
		}
	}
	return b, nil
}

func (p *memPV) RadiusSubscriber(_ context.Context, imsi string) (provisioner.RadiusSubscriber, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.radius[imsi]
	s.Rules = slices.Clone(s.Rules)
	return s, nil
}

func (p *memPV) write(ctx context.Context, op, key string) error {
	p.writes = append(p.writes, strings.Join([]string{op, key, provisioner.OperatorFrom(ctx)}, " "))
	return p.writeErr
}

func (p *memPV) UpdateRadiusSubscriber(ctx context.Context, imsi string, u provisioner.RadiusSubscriberUpdate, key string) (provisioner.RadiusSubscriber, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.write(ctx, "radius.update", key); err != nil {
		return provisioner.RadiusSubscriber{}, err
	}
	s := p.radius[imsi]
	s.Rules = u.Rules
	p.radius[imsi] = s
	return s, nil
}

func (p *memPV) SetState(ctx context.Context, d provisioner.Downstream, imsi string, ch provisioner.StateChange, key string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.write(ctx, string(d)+".state."+ch.State, key); err != nil {
		return "", err
	}
	if d == provisioner.Vecgen {
		p.vecgen[imsi] = ch.State
	} else {
		s := p.radius[imsi]
		s.State = ch.State
		p.radius[imsi] = s
	}
	return ch.State, nil
}

func (p *memPV) DeleteSubscriber(ctx context.Context, d provisioner.Downstream, imsi, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.write(ctx, string(d)+".delete", key); err != nil {
		return err
	}
	if d == provisioner.Vecgen {
		delete(p.vecgen, imsi)
	} else {
		delete(p.radius, imsi)
	}
	return nil
}

type env struct {
	s *memStore
	p *memPV
	r *Runner
}

func newEnv(maxFixes int) *env {
	s, p := newMemStore(), newMemPV()
	log := slog.New(slog.DiscardHandler)
	return &env{s: s, p: p, r: New(Options{Log: log, DB: s, PV: p, Audit: &audit.Recorder{DB: s, Log: log}, MaxFixes: maxFixes})}
}

func imsi(n int) string { return fmt.Sprintf("0010100%08d", n) }

// both は台帳と下流の両方に、ずれのない行を置く。
func (e *env) both(n int) {
	e.s.subs[imsi(n)] = db.Entry{IMSI: imsi(n), TenantID: 10, VecgenPresent: true, VecgenState: db.StateActive,
		RadiusPresent: true, RadiusState: db.StateActive, RadiusSourceID: "src"}
	e.p.vecgen[imsi(n)] = db.StateActive
	e.p.addRadius(imsi(n), db.StateActive, "src", 10)
}

func (e *env) run(t *testing.T) (db.ReconcileRun, string, error) {
	t.Helper()
	detail, err := e.r.RunScheduled(t.Context())
	return *e.s.runs[int64(len(e.s.runs))], detail, err
}

func TestCompare(t *testing.T) {
	live := db.Entry{IMSI: imsi(1), TenantID: 10, VecgenPresent: true, VecgenState: db.StateActive, RadiusPresent: true, RadiusState: db.StateActive, RadiusSourceID: "src"}
	vActive := view{present: true, state: db.StateActive}
	rActive := view{present: true, state: db.StateActive, source: "src", ruleTenants: []int64{10}}
	kinds := func(fs []db.Finding) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Kind+" "+string(f.Side))
		}
		return out
	}
	vecgenOnly := db.Entry{IMSI: imsi(1), TenantID: 10, VecgenPresent: true, VecgenState: db.StateActive}
	deleted := db.Entry{IMSI: imsi(1), TenantID: 20, DeletedAt: deletedAt}
	for _, tt := range []struct {
		name   string
		e      db.Entry
		exists bool
		v, r   view
		want   []string
		tenant int64
	}{
		{name: "ずれなし", e: live, exists: true, v: vActive, r: rActive},
		{name: "台帳にも下流にもない", e: db.Entry{}, exists: false},
		{name: "削除済みで下流にない", e: deleted, exists: true},
		{name: "台帳にあって下流にない", e: live, exists: true, r: rActive, want: []string{"ledger_only vecgen"}, tenant: 10},
		{name: "状態が違う", e: live, exists: true, v: view{present: true, state: db.StateInactive}, r: rActive, want: []string{"state_diff vecgen"}, tenant: 10},
		{name: "取得先が違う", e: live, exists: true, v: vActive, r: view{present: true, state: db.StateActive, source: "other", ruleTenants: []int64{10}}, want: []string{"source_diff radius"}, tenant: 10},
		{name: "その側だけ台帳にない", e: vecgenOnly, exists: true, v: vActive, r: rActive, want: []string{"side_absent radius"}, tenant: 10},
		{name: "ルールのテナントが違う", e: live, exists: true, v: vActive, r: view{present: true, state: db.StateActive, source: "src", ruleTenants: []int64{10, 20}}, want: []string{"rule_tenant radius"}, tenant: 10},
		{name: "IMSI が台帳にない（テナントの候補あり）", exists: false, v: vActive, r: view{present: true, state: db.StateActive, ruleTenants: []int64{20, 20}}, want: []string{"downstream_only vecgen", "downstream_only radius"}, tenant: 20},
		{name: "IMSI が台帳にない（テナントがそろわない）", exists: false, r: view{present: true, state: db.StateActive, ruleTenants: []int64{10, 20}}, want: []string{"downstream_only radius"}, tenant: 0},
		{name: "削除済みなのに下流にある（前のテナントが候補）", e: deleted, exists: true, v: vActive, want: []string{"deleted_present vecgen"}, tenant: 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := compare(tt.e, tt.exists, tt.v, tt.r, imsi(1))
			if !slices.Equal(kinds(got), tt.want) {
				t.Errorf("findings = %q, want %q", kinds(got), tt.want)
			}
			if len(got) > 0 && got[0].TenantID != tt.tenant {
				t.Errorf("tenant = %d, want %d", got[0].TenantID, tt.tenant)
			}
		})
	}
}

func TestRunFixesAndReports(t *testing.T) {
	e := newEnv(100)
	e.both(1) // ずれなし
	// 2: vecgen 側が下流にない。3: radius の状態が違う。4: 取得先が違う。5: radius 側が台帳にない。
	e.both(2)
	delete(e.p.vecgen, imsi(2))
	e.both(3)
	e.p.addRadius(imsi(3), db.StateInactive, "src", 10)
	e.both(4)
	e.p.addRadius(imsi(4), db.StateActive, "other", 10)
	e.s.subs[imsi(5)] = db.Entry{IMSI: imsi(5), TenantID: 10, VecgenPresent: true, VecgenState: db.StateActive}
	e.p.vecgen[imsi(5)] = db.StateActive
	e.p.addRadius(imsi(5), db.StateActive, "src", 10)
	// 6: 両側とも下流にない（削除済みにする）。
	e.s.subs[imsi(6)] = db.Entry{IMSI: imsi(6), TenantID: 10, VecgenPresent: true, VecgenState: db.StateInactive}
	// 7: IMSI が台帳にない（両側）。8: 台帳で削除済みなのに下流にある。9: ルールのテナントが違う。
	e.p.vecgen[imsi(7)] = db.StateActive
	e.p.addRadius(imsi(7), db.StateActive, "src", 20)
	e.s.subs[imsi(8)] = db.Entry{IMSI: imsi(8), TenantID: 20, DeletedAt: deletedAt}
	e.p.vecgen[imsi(8)] = db.StateInactive
	e.both(9)
	e.p.addRadius(imsi(9), db.StateActive, "src", 20)

	run, detail, err := e.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != db.RunCompleted || run.FixableCount != 5 || run.FixedCount != 5 || run.FindingsTotal != 9 || run.LedgerRows != 8 || run.VecgenRows != 7 || run.RadiusRows != 7 {
		t.Errorf("run = %+v", run)
	}
	if !strings.Contains(detail, "直した数 5") || !strings.Contains(detail, "未対応 3") {
		t.Errorf("detail = %q", detail)
	}
	// 台帳が下流に合う。
	if got := e.s.subs[imsi(2)]; got.VecgenPresent || !got.RadiusPresent {
		t.Errorf("2 = %+v", got)
	}
	if got := e.s.subs[imsi(3)]; got.RadiusState != db.StateInactive {
		t.Errorf("3 = %+v", got)
	}
	if got := e.s.subs[imsi(4)]; got.RadiusSourceID != "other" {
		t.Errorf("4 = %+v", got)
	}
	if got := e.s.subs[imsi(5)]; !got.RadiusPresent || got.RadiusSourceID != "src" || got.TenantID != 10 {
		t.Errorf("5 = %+v", got)
	}
	if got := e.s.subs[imsi(6)]; !got.Deleted() {
		t.Errorf("6 = %+v", got)
	}
	// 台帳にないものは自動で取り込まない。削除済みは戻さない。ルールは直さない。
	if _, ok := e.s.subs[imsi(7)]; ok {
		t.Error("7 was imported")
	}
	if got := e.s.subs[imsi(8)]; !got.Deleted() {
		t.Errorf("8 = %+v", got)
	}
	if got := e.p.radius[imsi(9)].Rules[0].TenantID; got != 20 {
		t.Errorf("9 rule tenant = %d", got)
	}
	if len(e.p.writes) != 0 {
		t.Errorf("wrote downstream: %v", e.p.writes)
	}
	for n, want := range map[int][]string{
		1: nil,
		2: {"ledger_only vecgen fixed"},
		3: {"state_diff radius fixed"},
		4: {"source_diff radius fixed"},
		5: {"side_absent radius fixed"},
		6: {"ledger_only vecgen fixed"},
		7: {"downstream_only radius pending", "downstream_only vecgen pending"},
		8: {"deleted_present vecgen pending"},
		9: {"rule_tenant radius info"},
	} {
		if got := e.s.findingTexts(imsi(n)); !slices.Equal(got, want) {
			t.Errorf("%d: findings = %q, want %q", n, got, want)
		}
	}
	// 補いの事実（理由つき。操作者は system、トレースは実行）。
	var reconciled int
	for _, ev := range e.s.events {
		if ev.Event != db.EventReconciled || ev.Reason != ReasonFix || ev.Operator != Operator || ev.TraceID != "reconcile-1" || ev.JobID != "" {
			t.Errorf("event = %+v", ev)
		}
		reconciled++
	}
	if reconciled != 4 { // 取得先だけの違いは事実にしない
		t.Errorf("events = %+v", e.s.events)
	}
	// 監査ログ: 実行ごとと、行ごと（テナントつき）。
	var actions []string
	for _, a := range e.s.audit {
		actions = append(actions, a.Action)
		if a.Action == audit.ActionSubscriberReconcile && (a.TenantID != 10 || a.ActorEmail != Operator || a.TraceID != "reconcile-1") {
			t.Errorf("audit = %+v", a)
		}
	}
	if actions[0] != audit.ActionReconcileStart || actions[len(actions)-1] != audit.ActionReconcileFinish || len(actions) != 7 {
		t.Errorf("audit actions = %q", actions)
	}
	// もう一度動かすと、自動で直すものはなくなる。
	run, _, err = e.run(t)
	if err != nil || run.FixableCount != 0 || run.FixedCount != 0 {
		t.Errorf("second run = %+v, %v", run, err)
	}
}

// 下流を 500 件ずつ、台帳を 1000 件ずつ読んでも、併合で取りこぼさない。
func TestRunPaging(t *testing.T) {
	e := newEnv(10)
	for n := 1; n <= 1203; n++ {
		e.both(n)
	}
	delete(e.p.vecgen, imsi(1001))
	run, _, err := e.run(t)
	if err != nil || run.LedgerRows != 1203 || run.VecgenRows != 1202 || run.RadiusRows != 1203 || run.FixedCount != 1 {
		t.Errorf("run = %+v, %v", run, err)
	}
	if got := e.s.subs[imsi(1001)]; got.VecgenPresent {
		t.Errorf("ledger = %+v", got)
	}
}

// 読み取りの間に変わったものは、読み直しで除く。
func TestConfirmDropsStaleCandidates(t *testing.T) {
	e := newEnv(0)
	e.both(1)
	// 一覧の写しには 1 の vecgen 側がないが、読み直すとある。上限が 0 でも保留にならない。
	e.p.vecgenStale = map[string]string{}
	run, _, err := e.run(t)
	if err != nil || run.State != db.RunCompleted || run.FindingsTotal != 0 || run.FixedCount != 0 {
		t.Errorf("run = %+v, %v", run, err)
	}
	if got := e.s.subs[imsi(1)]; !got.VecgenPresent {
		t.Errorf("ledger = %+v", got)
	}
	if len(e.s.events) != 0 {
		t.Errorf("events = %+v", e.s.events)
	}
}

// 処理中の操作（下流には書いたが、台帳はまだ）は、ロックの後に台帳を見直して、台帳にないと記録しない。
func TestConfirmSkipsInFlightOperation(t *testing.T) {
	e := newEnv(100)
	e.p.vecgen[imsi(1)] = db.StateActive
	e.s.onLock = func(i string) {
		e.s.mu.Lock()
		defer e.s.mu.Unlock()
		e.s.subs[i] = db.Entry{IMSI: i, TenantID: 10, VecgenPresent: true, VecgenState: db.StateActive}
	}
	run, _, err := e.run(t)
	if err != nil || run.FindingsTotal != 0 || run.State != db.RunCompleted {
		t.Errorf("run = %+v, %v", run, err)
	}
	if got := e.s.findingTexts(imsi(1)); len(got) != 0 {
		t.Errorf("findings = %q", got)
	}
}

// 安全弁: 上限を超えたら何も直さず保留。承認の実行は直す。
func TestHoldAndApprove(t *testing.T) {
	e := newEnv(2)
	for n := 1; n <= 3; n++ {
		e.both(n)
		delete(e.p.vecgen, imsi(n))
	}
	// 台帳にないものは数えない（直さないので）。
	e.p.vecgen[imsi(9)] = db.StateActive
	run, _, err := e.run(t)
	if err != nil || run.State != db.RunHeld || run.FixableCount != 3 || run.FixedCount != 0 || run.FindingsTotal != 4 {
		t.Fatalf("run = %+v, %v", run, err)
	}
	for n := 1; n <= 3; n++ {
		if got := e.s.subs[imsi(n)]; !got.VecgenPresent {
			t.Errorf("%d was changed: %+v", n, got)
		}
		if got := e.s.findingTexts(imsi(n)); !slices.Equal(got, []string{"ledger_only vecgen held"}) {
			t.Errorf("%d: %q", n, got)
		}
	}
	if len(e.s.events) != 0 {
		t.Errorf("events = %+v", e.s.events)
	}
	if !slices.ContainsFunc(e.s.audit, func(a db.AuditEntry) bool { return a.Action == audit.ActionReconcileHold }) {
		t.Errorf("audit = %+v", e.s.audit)
	}
	// 上限ちょうどは直す。
	e2 := newEnv(3)
	for n := 1; n <= 3; n++ {
		e2.both(n)
		delete(e2.p.vecgen, imsi(n))
	}
	if run, _, _ := e2.run(t); run.State != db.RunCompleted || run.FixedCount != 3 {
		t.Errorf("run at the limit = %+v", run)
	}

	// 承認: 読み直して直す（操作者はシステム管理者）。
	started, err := e.r.Start(t.Context(), db.TriggerApproved, "d@example.com", "バックアップから戻した")
	if err != nil {
		t.Fatal(err)
	}
	e.r.Wait()
	got := *e.s.runs[started.ID]
	if got.State != db.RunCompleted || got.FixedCount != 3 || got.ApproveReason != "バックアップから戻した" {
		t.Errorf("approved run = %+v", got)
	}
	if e.s.subs[imsi(1)].VecgenPresent {
		t.Error("not fixed")
	}
	if ev := e.s.events[0]; ev.Operator != "d@example.com" {
		t.Errorf("event = %+v", ev)
	}
}

// 一覧の読み取りが途中で失敗したら何も直さない。
func TestScanFailureFixesNothing(t *testing.T) {
	e := newEnv(100)
	for n := 1; n <= 1100; n++ {
		e.both(n)
	}
	delete(e.p.vecgen, imsi(1))
	e.p.listErrAfter = 3
	run, _, err := e.run(t)
	if err == nil || run.State != db.RunFailed || !strings.Contains(run.Error, "ここで止めました") || run.FixedCount != 0 {
		t.Fatalf("run = %+v, %v", run, err)
	}
	if !e.s.subs[imsi(1)].VecgenPresent || len(e.s.events) != 0 {
		t.Errorf("changed the ledger: %+v, %+v", e.s.subs[imsi(1)], e.s.events)
	}
	// 台帳の読み取りの失敗も同じ。
	e = newEnv(100)
	e.s.entriesErr = errors.New("db is down")
	if run, _, err := e.run(t); err == nil || run.State != db.RunFailed {
		t.Errorf("run = %+v, %v", run, err)
	}
}

// 修正の途中で下流を読めなくなったら止める（直した分は記録に残る）。
func TestFixStopsWhenUnreadable(t *testing.T) {
	e := newEnv(100)
	e.both(1)
	delete(e.p.vecgen, imsi(1))
	// 読み直しの段（confirm）の後に誤りにする。
	calls := 0
	wrap := &failingBatch{memPV: e.p, failAfter: 1, calls: &calls}
	log := slog.New(slog.DiscardHandler)
	r := New(Options{Log: log, DB: e.s, PV: wrap, Audit: &audit.Recorder{DB: e.s, Log: log}, MaxFixes: 100})
	if _, err := r.RunScheduled(t.Context()); err == nil {
		t.Fatal("no error")
	}
	if got := e.s.runs[1]; got.State != db.RunFailed || got.FixedCount != 0 {
		t.Errorf("run = %+v", got)
	}
	if !e.s.subs[imsi(1)].VecgenPresent {
		t.Error("changed the ledger without reading")
	}
}

// failingBatch は一括取得を failAfter 回の後に失敗させる。
type failingBatch struct {
	*memPV
	failAfter int
	calls     *int
}

func (f *failingBatch) VecgenBatchGet(ctx context.Context, imsis []string) (provisioner.VecgenBatch, error) {
	*f.calls++
	if *f.calls > f.failAfter {
		return provisioner.VecgenBatch{}, errors.New("unreachable")
	}
	return f.memPV.VecgenBatchGet(ctx, imsis)
}

func TestInterrupted(t *testing.T) {
	e := newEnv(100)
	for n := 1; n <= 600; n++ {
		e.both(n)
	}
	delete(e.p.vecgen, imsi(1))
	ctx, cancel := context.WithCancel(context.Background())
	e.p.onList = cancel
	// 取り消しを見て一覧の読み取りをやめるよう、memPV の一覧に代わって誤りを返す。
	e.p.listErrAfter = 1
	_, err := e.r.RunScheduled(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := e.s.runs[1]; got.State != db.RunInterrupted || got.FixedCount != 0 {
		t.Errorf("run = %+v", got)
	}
	if !e.s.subs[imsi(1)].VecgenPresent {
		t.Error("changed the ledger")
	}
	// 起動のとき: 実行中のまま残ったものを中断にする。
	e.s.runs[2] = &db.ReconcileRun{ID: 2, State: db.RunRunning}
	if err := e.r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.s.runs[2].State != db.RunInterrupted {
		t.Errorf("run = %+v", e.s.runs[2])
	}
}

func TestNotConcurrent(t *testing.T) {
	e := newEnv(100)
	e.both(1)
	gate := make(chan struct{})
	entered := make(chan struct{}, 10)
	e.p.onList = func() {
		entered <- struct{}{}
		<-gate
	}
	if _, err := e.r.Start(t.Context(), db.TriggerManual, "d@example.com", ""); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := e.r.Start(t.Context(), db.TriggerManual, "d@example.com", ""); !errors.Is(err, ErrRunning) {
		t.Errorf("second start = %v", err)
	}
	// 定刻の実行は飛ばす（誤りにしない）。
	detail, err := e.r.RunScheduled(t.Context())
	if err != nil || !strings.Contains(detail, "飛ばしました") {
		t.Errorf("scheduled = %q, %v", detail, err)
	}
	close(gate)
	e.r.Wait()
	if len(e.s.runs) != 1 || e.s.runs[1].State != db.RunCompleted {
		t.Errorf("runs = %+v", e.s.runs[1])
	}
	// 終われば次を動かせる。別のプロセスがロックを持っていれば ErrRunning。
	e.s.locked = true
	if _, err := e.r.Start(t.Context(), db.TriggerManual, "d@example.com", ""); !errors.Is(err, ErrRunning) {
		t.Errorf("locked = %v", err)
	}
}

func TestManualRunActor(t *testing.T) {
	e := newEnv(100)
	e.both(1)
	delete(e.p.vecgen, imsi(1))
	if _, err := e.r.Start(t.Context(), db.TriggerManual, "d@example.com", ""); err != nil {
		t.Fatal(err)
	}
	e.r.Wait()
	if ev := e.s.events[0]; ev.Operator != "d@example.com" {
		t.Errorf("event = %+v", ev)
	}
	if a := e.s.audit[0]; a.ActorEmail != "d@example.com" || a.ActorRole != "system_admin" {
		t.Errorf("audit = %+v", a)
	}
}

func findingCtx() context.Context {
	return provisioner.WithOperator(trace.With(context.Background(), "t1"), "d@example.com")
}

func TestImport(t *testing.T) {
	t.Run("両側を取り込み、ルールのテナントを書き換える", func(t *testing.T) {
		e := newEnv(100)
		e.p.vecgen[imsi(1)] = db.StateActive
		e.p.addRadius(imsi(1), db.StateInactive, "src", 20, 10)
		if _, _, err := e.run(t); err != nil {
			t.Fatal(err)
		}
		f := e.s.findings[0]
		st, err := e.r.Import(findingCtx(), f, 10, "d@example.com")
		if err != nil || st != db.FindingImported {
			t.Fatalf("import = %q, %v", st, err)
		}
		got := e.s.subs[imsi(1)]
		if got.TenantID != 10 || got.VecgenState != db.StateActive || got.RadiusState != db.StateInactive || got.RadiusSourceID != "src" || got.RegisteredBy != "d@example.com" {
			t.Errorf("ledger = %+v", got)
		}
		if rules := e.p.radius[imsi(1)].Rules; rules[0].TenantID != 10 || rules[1].TenantID != 10 {
			t.Errorf("rules = %+v", rules)
		}
		if !slices.Equal(e.p.writes, []string{fmt.Sprintf("radius.update reconcile-%d-rules-10 d@example.com", f.ID)}) {
			t.Errorf("writes = %v", e.p.writes)
		}
		if got := e.s.findingTexts(imsi(1)); !slices.Equal(got, []string{"downstream_only radius imported", "downstream_only vecgen imported"}) {
			t.Errorf("findings = %q", got)
		}
		for _, ev := range e.s.events {
			if ev.Event != db.EventReconciled || ev.Reason != ReasonImport || ev.Operator != "d@example.com" || ev.TraceID != "t1" {
				t.Errorf("event = %+v", ev)
			}
		}
		if len(e.s.events) != 2 {
			t.Errorf("events = %+v", e.s.events)
		}
	})
	t.Run("ルールのテナントが同じなら書き換えない", func(t *testing.T) {
		e := newEnv(100)
		e.p.addRadius(imsi(1), db.StateActive, "src", 10)
		e.run(t)
		if st, err := e.r.Import(findingCtx(), e.s.findings[0], 10, "d@example.com"); err != nil || st != db.FindingImported {
			t.Fatalf("import = %q, %v", st, err)
		}
		if len(e.p.writes) != 0 {
			t.Errorf("writes = %v", e.p.writes)
		}
	})
	t.Run("解約のものはルールを書き換えない", func(t *testing.T) {
		e := newEnv(100)
		e.p.addRadius(imsi(1), db.StateTerminated, "src", 20)
		e.run(t)
		if st, err := e.r.Import(findingCtx(), e.s.findings[0], 10, "d@example.com"); err != nil || st != db.FindingImported {
			t.Fatalf("import = %q, %v", st, err)
		}
		if len(e.p.writes) != 0 || e.s.subs[imsi(1)].RadiusState != db.StateTerminated {
			t.Errorf("writes = %v, ledger = %+v", e.p.writes, e.s.subs[imsi(1)])
		}
	})
	t.Run("削除済みを取り込む", func(t *testing.T) {
		e := newEnv(100)
		e.s.subs[imsi(1)] = db.Entry{IMSI: imsi(1), TenantID: 20, DeletedAt: deletedAt}
		e.p.vecgen[imsi(1)] = db.StateInactive
		e.run(t)
		if st, err := e.r.Import(findingCtx(), e.s.findings[0], 10, "d@example.com"); err != nil || st != db.FindingImported {
			t.Fatalf("import = %q, %v", st, err)
		}
		if got := e.s.subs[imsi(1)]; got.Deleted() || got.TenantID != 10 {
			t.Errorf("ledger = %+v", got)
		}
	})
	t.Run("解消していれば何もしない", func(t *testing.T) {
		e := newEnv(100)
		e.p.vecgen[imsi(1)] = db.StateActive
		e.run(t)
		f := e.s.findings[0]
		// 台帳に入った場合。
		e.s.subs[imsi(1)] = db.Entry{IMSI: imsi(1), TenantID: 20, VecgenPresent: true, VecgenState: db.StateActive}
		if st, err := e.r.Import(findingCtx(), f, 10, "d@example.com"); err != nil || st != db.FindingGone {
			t.Errorf("import = %q, %v", st, err)
		}
		if got := e.s.subs[imsi(1)]; got.TenantID != 20 {
			t.Errorf("ledger = %+v", got)
		}
		// 下流からなくなった場合。
		e2 := newEnv(100)
		e2.p.vecgen[imsi(1)] = db.StateActive
		e2.run(t)
		delete(e2.p.vecgen, imsi(1))
		if st, err := e2.r.Import(findingCtx(), e2.s.findings[0], 10, "d@example.com"); err != nil || st != db.FindingGone {
			t.Errorf("import = %q, %v", st, err)
		}
		if _, ok := e2.s.subs[imsi(1)]; ok {
			t.Error("ledger has a row")
		}
	})
	t.Run("失敗は未対応のまま", func(t *testing.T) {
		e := newEnv(100)
		e.p.addRadius(imsi(1), db.StateActive, "src", 20)
		e.run(t)
		f := e.s.findings[0]
		// 書き換えの失敗。
		e.p.writeErr = errors.New("unreachable")
		if _, err := e.r.Import(findingCtx(), f, 10, "d@example.com"); err == nil {
			t.Error("no error")
		}
		e.p.writeErr = nil
		// 読めない。
		e.p.batchErr = errors.New("unreachable")
		if _, err := e.r.Import(findingCtx(), f, 10, "d@example.com"); !errors.Is(err, ErrUnreadable) {
			t.Errorf("err = %v", err)
		}
		e.p.batchErr = nil
		// 削除済みのテナント。
		if _, err := e.r.Import(findingCtx(), f, 99, "d@example.com"); !errors.Is(err, db.ErrTenantNotFound) {
			t.Errorf("err = %v", err)
		}
		if _, ok := e.s.subs[imsi(1)]; ok {
			t.Error("ledger has a row")
		}
		if got := e.s.findingTexts(imsi(1)); !slices.Equal(got, []string{"downstream_only radius pending"}) {
			t.Errorf("findings = %q", got)
		}
	})
}

func TestDelete(t *testing.T) {
	t.Run("Active は止めてから消す", func(t *testing.T) {
		e := newEnv(100)
		e.p.vecgen[imsi(1)] = db.StateActive
		e.p.addRadius(imsi(1), db.StateActive, "src", 10)
		e.run(t)
		var vf db.Finding
		for _, f := range e.s.findings {
			if f.Side == db.SideVecgen {
				vf = f
			}
		}
		st, err := e.r.Delete(findingCtx(), vf, "d@example.com")
		if err != nil || st != db.FindingDeleted {
			t.Fatalf("delete = %q, %v", st, err)
		}
		want := []string{fmt.Sprintf("vecgen.state.inactive reconcile-%d-state d@example.com", vf.ID), fmt.Sprintf("vecgen.delete reconcile-%d-delete d@example.com", vf.ID)}
		if !slices.Equal(e.p.writes, want) {
			t.Errorf("writes = %v", e.p.writes)
		}
		if _, ok := e.p.vecgen[imsi(1)]; ok {
			t.Error("not deleted")
		}
		// その側だけ。台帳と事実は書かない。
		if _, ok := e.p.radius[imsi(1)]; !ok {
			t.Error("the other side was deleted")
		}
		if got := e.s.findingTexts(imsi(1)); !slices.Equal(got, []string{"downstream_only radius pending", "downstream_only vecgen deleted"}) {
			t.Errorf("findings = %q", got)
		}
		if _, ok := e.s.subs[imsi(1)]; ok || len(e.s.events) != 0 {
			t.Errorf("ledger was written: %+v", e.s.events)
		}
	})
	t.Run("Inactive はそのまま消す", func(t *testing.T) {
		e := newEnv(100)
		e.p.vecgen[imsi(1)] = db.StateInactive
		e.run(t)
		if st, err := e.r.Delete(findingCtx(), e.s.findings[0], "d@example.com"); err != nil || st != db.FindingDeleted {
			t.Fatalf("delete = %q, %v", st, err)
		}
		if len(e.p.writes) != 1 {
			t.Errorf("writes = %v", e.p.writes)
		}
	})
	t.Run("解消していれば消さない", func(t *testing.T) {
		e := newEnv(100)
		e.p.vecgen[imsi(1)] = db.StateInactive
		e.run(t)
		f := e.s.findings[0]
		e.s.subs[imsi(1)] = db.Entry{IMSI: imsi(1), TenantID: 10, VecgenPresent: true, VecgenState: db.StateInactive}
		if st, err := e.r.Delete(findingCtx(), f, "d@example.com"); err != nil || st != db.FindingGone || len(e.p.writes) != 0 {
			t.Errorf("delete = %q, %v, writes %v", st, err, e.p.writes)
		}
		e2 := newEnv(100)
		e2.p.vecgen[imsi(1)] = db.StateInactive
		e2.run(t)
		delete(e2.p.vecgen, imsi(1))
		if st, err := e2.r.Delete(findingCtx(), e2.s.findings[0], "d@example.com"); err != nil || st != db.FindingGone || len(e2.p.writes) != 0 {
			t.Errorf("delete = %q, %v, writes %v", st, err, e2.p.writes)
		}
	})
	t.Run("失敗は未対応のまま", func(t *testing.T) {
		e := newEnv(100)
		e.p.vecgen[imsi(1)] = db.StateInactive
		e.run(t)
		e.p.writeErr = errors.New("unreachable")
		if _, err := e.r.Delete(findingCtx(), e.s.findings[0], "d@example.com"); err == nil {
			t.Error("no error")
		}
		if got := e.s.findingTexts(imsi(1)); !slices.Equal(got, []string{"downstream_only vecgen pending"}) {
			t.Errorf("findings = %q", got)
		}
	})
}

func TestCheckMarked(t *testing.T) {
	e := newEnv(100)
	// 1: 下流では Inactive。2: 登録の結果を確かめていて、どちらの側にもない。
	e.both(1)
	e.p.vecgen[imsi(1)] = db.StateInactive
	marked := e.s.subs[imsi(1)]
	marked.CheckNeeded = true
	e.s.subs[imsi(1)] = marked
	e.s.subs[imsi(2)] = db.Entry{IMSI: imsi(2), TenantID: 20, CheckNeeded: true}
	// 読めない間は何もしない。
	e.p.batchErr = errors.New("unreachable")
	if detail, err := e.r.CheckMarked(t.Context()); err != nil || detail != "" {
		t.Errorf("unreadable = %q, %v", detail, err)
	}
	if !e.s.subs[imsi(1)].CheckNeeded {
		t.Error("mark was removed")
	}
	e.p.batchErr = nil
	detail, err := e.r.CheckMarked(t.Context())
	if err != nil || !strings.Contains(detail, "2 件") {
		t.Errorf("detail = %q, %v", detail, err)
	}
	if got := e.s.subs[imsi(1)]; got.CheckNeeded || got.VecgenState != db.StateInactive {
		t.Errorf("ledger = %+v", got)
	}
	if _, ok := e.s.subs[imsi(2)]; ok {
		t.Error("pending row remains")
	}
	var tenants []int64
	for _, a := range e.s.audit {
		if a.Action != audit.ActionSubscriberCheck || a.ActorEmail != Operator || a.TraceID != "check-needed" {
			t.Errorf("audit = %+v", a)
		}
		tenants = append(tenants, a.TenantID)
	}
	if !slices.Equal(tenants, []int64{10, 20}) {
		t.Errorf("audit tenants = %v", tenants)
	}
	// 印がなければ何も書かない。
	if detail, err := e.r.CheckMarked(t.Context()); err != nil || detail != "" {
		t.Errorf("nothing = %q, %v", detail, err)
	}
	// 1 回に 100 件まで。
	for n := 1000; n < 1150; n++ {
		e.s.subs[imsi(n)] = db.Entry{IMSI: imsi(n), TenantID: 10, CheckNeeded: true}
	}
	if detail, _ := e.r.CheckMarked(t.Context()); !strings.Contains(detail, "100 件") {
		t.Errorf("limit = %q", detail)
	}
}

func TestFindingsAreCapped(t *testing.T) {
	st := &runState{r: &Runner{}, run: db.ReconcileRun{}}
	for range maxFindings + 5 {
		st.add(db.Finding{Status: db.FindingPending})
	}
	if st.run.FindingsTotal != maxFindings+5 || st.pending != maxFindings+5 || len(st.buf) != maxFindings {
		t.Errorf("total = %d, pending = %d, stored = %d", st.run.FindingsTotal, st.pending, len(st.buf))
	}
}
