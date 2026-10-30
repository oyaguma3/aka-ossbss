package bulk

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/kek"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

var time1 = time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)

// memStore はメモリーの DB。台帳は db.Transition で書く（本物の DB と同じ移り方）。
type memStore struct {
	mu      sync.Mutex
	subs    map[string]db.Entry
	events  []db.ChargingEvent
	jobs    map[int64]*db.Job
	sealed  map[int64][]byte
	deleted map[int64][]string
	rows    map[int64][]db.JobRow
	audit   []db.AuditEntry
	tenants []db.Tenant
	locks   map[db.Side]bool
	nextID  int64
	// subsErr を設定すると WithSubscriber が誤りを返す。
	subsErr error
	now     time.Time
}

func newMemStore() *memStore {
	return &memStore{subs: map[string]db.Entry{}, jobs: map[int64]*db.Job{}, sealed: map[int64][]byte{}, deleted: map[int64][]string{},
		rows: map[int64][]db.JobRow{}, tenants: []db.Tenant{{ID: 10, Name: "A"}, {ID: 20, Name: "B"}}, locks: map[db.Side]bool{}, now: time1}
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
	l.cur, l.exists = res.Entry, !res.Remove
	l.events = append(l.events, res.Events...)
	return nil
}

func (s *memStore) WithSubscriber(_ context.Context, imsi string, meta db.Meta, fn func(db.Ledger) error) error {
	s.mu.Lock()
	if s.subsErr != nil {
		defer s.mu.Unlock()
		return s.subsErr
	}
	cur, ok := s.subs[imsi]
	if !ok {
		cur = db.Entry{IMSI: imsi}
	}
	s.mu.Unlock()
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

func (s *memStore) Tenants(context.Context) ([]db.Tenant, error) { return s.tenants, nil }

func (s *memStore) NextQueuedJob(_ context.Context, target db.Side) (db.Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(s.jobs)) {
		if j := s.jobs[id]; j.State == db.JobQueued && j.Target == target {
			return *j, true, nil
		}
	}
	return db.Job{}, false, nil
}

func (s *memStore) move(id int64, from string, fn func(*db.Job)) (db.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return db.Job{}, db.ErrNotFound
	}
	if j.State != from {
		return db.Job{}, db.ErrConflict
	}
	fn(j)
	out := *j
	out.Sealed = s.sealed[id]
	return out, nil
}

func (s *memStore) StartJob(_ context.Context, id int64) (db.Job, error) {
	return s.move(id, db.JobQueued, func(j *db.Job) { j.State = db.JobRunning })
}

func (s *memStore) TakeQueuedJob(_ context.Context, id int64, by, reason string) (db.Job, error) {
	return s.move(id, db.JobQueued, func(j *db.Job) {
		j.State, j.InterruptBy, j.Reason, j.Unprocessed = db.JobInterrupted, by, reason, j.Total
	})
}

func (s *memStore) RequestJobInterrupt(_ context.Context, id int64, by string) (db.Job, error) {
	return s.move(id, db.JobRunning, func(j *db.Job) { j.InterruptBy = by })
}

func (s *memStore) FlushJobProgress(_ context.Context, id int64, d db.JobCounts, rows []db.JobRow) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	j.OK += d.OK
	j.Exists += d.Exists
	j.Absent += d.Absent
	j.Failed += d.Failed
	j.Unprocessed += d.Unprocessed
	j.Excluded += d.Excluded
	s.rows[id] = append(s.rows[id], rows...)
	return j.InterruptBy, nil
}

func (s *memStore) FinishJob(_ context.Context, id int64, state, reason string) (db.Job, error) {
	return s.move(id, db.JobRunning, func(j *db.Job) { j.State, j.Reason = state, reason })
}

func (s *memStore) InterruptRunningJobs(_ context.Context, reason string) ([]db.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []db.Job
	for _, id := range slices.Sorted(maps.Keys(s.jobs)) {
		if j := s.jobs[id]; j.State == db.JobRunning {
			j.State, j.Reason = db.JobInterrupted, reason
			j.Unprocessed = j.Total - j.Done()
			out = append(out, *j)
		}
	}
	return out, nil
}

func (s *memStore) ExpireValidatedJobs(_ context.Context, before time.Time, reason string) ([]db.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []db.Job
	for _, id := range slices.Sorted(maps.Keys(s.jobs)) {
		if j := s.jobs[id]; j.State == db.JobValidated && j.CreatedAt.Before(before) {
			j.State, j.Reason = db.JobCanceled, reason
			out = append(out, *j)
		}
	}
	return out, nil
}

func (s *memStore) JobDeletedIMSIs(_ context.Context, id int64) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted[id], nil
}

func (s *memStore) TryJobLock(_ context.Context, target db.Side) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[target] {
		return nil, false, nil
	}
	s.locks[target] = true
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.locks[target] = false
	}, true, nil
}

// job は今のジョブの写しを返す。
func (s *memStore) job(id int64) db.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.jobs[id]
}

func (s *memStore) jobRows(id int64) []db.JobRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.rows[id])
	slices.SortFunc(out, func(a, b db.JobRow) int { return a.Line - b.Line })
	return out
}

func (s *memStore) entry(imsi string) (db.Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.subs[imsi]
	return e, ok
}

// memPV はメモリーの aka-provisioner。
type memPV struct {
	mu       sync.Mutex
	vecgen   map[string]string
	radius   map[string]string
	calls    []string
	inFlight int
	maxIn    int
	// fail は呼び出しごとの誤りを決める（nil を返せば反映する）。applied なら、反映したうえで誤りを返す。
	fail func(op, imsi string) (err error, applied bool)
	// gate を設定すると、書き込みはこのチャネルを受けるまで止まる。
	gate chan struct{}
	// entered は書き込みに入ったことの知らせ（設定したときだけ）。
	entered chan string
	// batchErr を設定すると一括取得が誤りを返す。
	batchErr error
	last     provisioner.VecgenSubscriberCreate
	rules    []provisioner.Rule
}

func newMemPV() *memPV { return &memPV{vecgen: map[string]string{}, radius: map[string]string{}} }

func (p *memPV) write(ctx context.Context, op, imsi, key string, apply func()) error {
	p.mu.Lock()
	p.calls = append(p.calls, strings.Join([]string{op, imsi, key, provisioner.OperatorFrom(ctx), trace.From(ctx)}, " "))
	p.inFlight++
	p.maxIn = max(p.maxIn, p.inFlight)
	gate, entered := p.gate, p.entered
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()
	if entered != nil {
		entered <- imsi
	}
	if gate != nil {
		<-gate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		if err, applied := p.fail(op, imsi); err != nil {
			if applied {
				apply()
			}
			return err
		}
	}
	apply()
	return nil
}

func pvErr(status int, cause string) error {
	return &provisioner.Error{Status: status, Problem: provisioner.Problem{Status: status, Cause: cause}}
}

func (p *memPV) CreateVecgenSubscriber(ctx context.Context, s provisioner.VecgenSubscriberCreate, key string) (provisioner.VecgenSubscriber, error) {
	var exists bool
	err := p.write(ctx, "vecgen.create", s.IMSI, key, func() {
		if _, exists = p.vecgen[s.IMSI]; !exists {
			p.vecgen[s.IMSI] = s.State
			p.last = s
		}
	})
	if err == nil && exists {
		err = pvErr(409, provisioner.CauseSubscriberExists)
	}
	return provisioner.VecgenSubscriber{}, err
}

func (p *memPV) CreateRadiusSubscriber(ctx context.Context, s provisioner.RadiusSubscriberCreate, key string) (provisioner.RadiusSubscriber, error) {
	var exists bool
	err := p.write(ctx, "radius.create", s.IMSI, key, func() {
		if _, exists = p.radius[s.IMSI]; !exists {
			p.radius[s.IMSI] = s.State
			p.rules = s.Rules
		}
	})
	if err == nil && exists {
		err = pvErr(409, provisioner.CauseSubscriberExists)
	}
	return provisioner.RadiusSubscriber{}, err
}

func (p *memPV) DeleteSubscriber(ctx context.Context, d provisioner.Downstream, imsi, key string) error {
	var missing bool
	err := p.write(ctx, string(d)+".delete", imsi, key, func() {
		m := p.vecgen
		if d == provisioner.Radius {
			m = p.radius
		}
		if _, ok := m[imsi]; !ok {
			missing = true
			return
		}
		delete(m, imsi)
	})
	if err == nil && missing {
		err = pvErr(404, provisioner.CauseUserNotFound)
	}
	return err
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
		if st, ok := p.radius[imsi]; ok {
			b.Items = append(b.Items, provisioner.RadiusSubscriber{IMSI: imsi, State: st, SourceID: "vecgen"})
		}
	}
	return b, nil
}

func (p *memPV) callList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// env は試験の環境。
type env struct {
	s *memStore
	p *memPV
	k *kek.KEK
	r *Runner
}

func newEnv(t *testing.T, concurrency int) *env {
	t.Helper()
	k, err := kek.New(bytes.Repeat([]byte{7}, kek.Size))
	if err != nil {
		t.Fatal(err)
	}
	s, p := newMemStore(), newMemPV()
	log := slog.New(slog.DiscardHandler)
	r := New(Options{Log: log, DB: s, PV: p, KEK: k, Audit: &audit.Recorder{DB: s, Log: log}, Concurrency: concurrency,
		FlushInterval: 5 * time.Millisecond, Now: func() time.Time { return s.now }})
	return &env{s: s, p: p, k: k, r: r}
}

// queue は待ちのジョブを置く。
func (e *env) queue(t *testing.T, kind Kind, st Settings, rows []string, mod ...func(*db.Job)) int64 {
	t.Helper()
	var b strings.Builder
	b.WriteString(strings.Join(kind.Header(), ",") + "\n")
	for _, r := range rows {
		b.WriteString(r + "\n")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	e.s.mu.Lock()
	defer e.s.mu.Unlock()
	e.s.nextID++
	id := e.s.nextID
	sealed, err := e.k.Seal([]byte(b.String()), AAD(id))
	if err != nil {
		t.Fatal(err)
	}
	j := &db.Job{ID: id, Kind: string(kind), Target: kind.Side(), State: db.JobQueued, Settings: raw, FileName: "a.csv", Total: len(rows), CreatedBy: "d@example.com"}
	for _, m := range mod {
		m(j)
	}
	e.s.jobs[id] = j
	e.s.sealed[id] = sealed
	return id
}

func (e *env) run(t *testing.T, target db.Side) {
	t.Helper()
	ran, err := e.r.RunNext(t.Context(), target)
	if err != nil || !ran {
		t.Fatalf("RunNext = %v, %v", ran, err)
	}
}

func imsi(n int) string { return fmt.Sprintf("0010100000%05d", n) }

func vecgenLine(n int) string { return imsi(n) + "," + testKi + "," + testOPc + ",," }

var vecgenSettings = Settings{TenantID: 10, State: db.StateActive, SQNType: "inc32", AllowedClientIDs: []int64{1}}

func radiusSettings() Settings {
	return Settings{TenantID: 10, State: db.StateInactive, SourceID: "vecgen",
		Rules: []provisioner.Rule{{TenantID: 10, NASID: "*", SSIDs: []string{"*"}}}}
}

func wantCounts(t *testing.T, j db.Job, want db.JobCounts) {
	t.Helper()
	if j.JobCounts != want {
		t.Errorf("counts = %+v, want %+v", j.JobCounts, want)
	}
}

func TestRunVecgenCreate(t *testing.T) {
	e := newEnv(t, 2)
	// 1: 新しい、2: 台帳に既にある、3: 下流にだけある（409）、4: 検証の後に別のテナントに属した。
	e.s.subs[imsi(2)] = db.Entry{IMSI: imsi(2), TenantID: 10, VecgenPresent: true, VecgenState: db.StateActive}
	e.p.vecgen[imsi(3)] = db.StateActive
	e.s.subs[imsi(4)] = db.Entry{IMSI: imsi(4), TenantID: 20, RadiusPresent: true, RadiusState: db.StateActive, RadiusSourceID: "v"}
	id := e.queue(t, VecgenCreate, vecgenSettings, []string{vecgenLine(1), vecgenLine(2), vecgenLine(3), vecgenLine(4)})
	e.run(t, db.SideVecgen)

	j := e.s.job(id)
	if j.State != db.JobCompleted || j.Reason != "" {
		t.Errorf("state = %s (%s)", j.State, j.Reason)
	}
	wantCounts(t, j, db.JobCounts{OK: 1, Exists: 2, Failed: 1})
	got, ok := e.s.entry(imsi(1))
	if !ok || got.TenantID != 10 || got.VecgenState != db.StateActive || got.RegisteredBy != "d@example.com" {
		t.Errorf("ledger = %+v, %v", got, ok)
	}
	if _, ok := e.s.entry(imsi(3)); ok {
		t.Error("ledger has the row that exists only downstream")
	}
	// 既にある行（台帳）は下流を呼ばない。キー・操作者・トレースID。
	calls := e.p.callList()
	if len(calls) != 2 || !slices.Contains(calls, "vecgen.create "+imsi(1)+" job-1-"+imsi(1)+" d@example.com job-1") {
		t.Errorf("calls = %q", calls)
	}
	// 下流に送るのは小文字にした鍵と既定の AMF・SQN。
	if e.p.last.Ki != strings.ToLower(testKi) || e.p.last.AMF != DefaultAMF || e.p.last.SQN != DefaultSQN || !slices.Equal(e.p.last.AllowedClientIDs, []int64{1}) {
		t.Errorf("sent = %+v", e.p.last)
	}
	rows := e.s.jobRows(id)
	if len(rows) != 3 || rows[0].Result != db.RowExists || rows[0].Line != 3 || rows[2].Result != db.RowFailed || !strings.Contains(rows[2].Reason, "別のテナント") {
		t.Errorf("rows = %+v", rows)
	}
	// 状態の事実にジョブの ID。監査ログに鍵が入らない。
	if len(e.s.events) != 1 || e.s.events[0].JobID != "1" || e.s.events[0].TraceID != "job-1" || e.s.events[0].Event != db.EventLineRegistered {
		t.Errorf("events = %+v", e.s.events)
	}
	var actions []string
	for _, a := range e.s.audit {
		actions = append(actions, a.Action)
		if a.JobID != "1" || a.TraceID != "job-1" || a.ActorEmail != "d@example.com" {
			t.Errorf("audit = %+v", a)
		}
		if strings.Contains(fmt.Sprint(a.Detail), strings.ToLower(testKi)) {
			t.Errorf("audit contains the key: %+v", a)
		}
	}
	if !slices.Equal(actions, []string{audit.ActionJobStart, audit.ActionVecgenCreate, audit.ActionJobFinish}) {
		t.Errorf("audit actions = %q", actions)
	}
}

func TestRunRadiusCreateUsesSettings(t *testing.T) {
	e := newEnv(t, 5)
	id := e.queue(t, RadiusCreate, radiusSettings(), []string{imsi(1)})
	e.run(t, db.SideRadius)
	wantCounts(t, e.s.job(id), db.JobCounts{OK: 1})
	got, _ := e.s.entry(imsi(1))
	if got.RadiusSourceID != "vecgen" || got.RadiusState != db.StateInactive || got.TenantID != 10 {
		t.Errorf("ledger = %+v", got)
	}
	if len(e.p.rules) != 1 || e.p.rules[0].TenantID != 10 {
		t.Errorf("rules = %+v", e.p.rules)
	}
}

func TestRunDelete(t *testing.T) {
	e := newEnv(t, 5)
	// 1: 消せる、2: 台帳にない、3: 下流にない（404）、4: 検証の後に Active になった、5: 消せる（Terminated）。
	put := func(n int, state string) {
		e.s.subs[imsi(n)] = db.Entry{IMSI: imsi(n), TenantID: 10, VecgenPresent: true, VecgenState: state, RadiusPresent: true, RadiusState: db.StateInactive, RadiusSourceID: "v"}
	}
	put(1, db.StateInactive)
	e.p.vecgen[imsi(1)] = db.StateInactive
	put(3, db.StateInactive)
	put(4, db.StateActive)
	put(5, db.StateTerminated)
	e.p.vecgen[imsi(5)] = db.StateTerminated
	id := e.queue(t, VecgenDelete, Settings{}, []string{imsi(1), imsi(2), imsi(3), imsi(4), imsi(5)})
	e.run(t, db.SideVecgen)
	wantCounts(t, e.s.job(id), db.JobCounts{OK: 2, Absent: 2, Failed: 1})
	got, _ := e.s.entry(imsi(1))
	if got.VecgenPresent || !got.RadiusPresent || got.Deleted() {
		t.Errorf("ledger after delete = %+v", got)
	}
	if _, ok := e.p.vecgen[imsi(1)]; ok {
		t.Error("not deleted downstream")
	}
	if c := e.p.callList(); len(c) != 3 {
		t.Errorf("calls = %q (no call for rows the ledger refused)", c)
	}
	// 片側を消しても両側がなくならない。もう一方を消すと台帳は削除済み。
	id2 := e.queue(t, RadiusDelete, Settings{}, []string{imsi(1)})
	e.p.radius[imsi(1)] = db.StateInactive
	e.run(t, db.SideRadius)
	wantCounts(t, e.s.job(id2), db.JobCounts{OK: 1})
	if got, _ := e.s.entry(imsi(1)); !got.Deleted() {
		t.Errorf("ledger after both = %+v", got)
	}
}

func TestDeletedChoice(t *testing.T) {
	deleted := func(e *env, n int) {
		e.s.subs[imsi(n)] = db.Entry{IMSI: imsi(n), TenantID: 20, DeletedAt: time1}
	}
	t.Run("含める", func(t *testing.T) {
		e := newEnv(t, 5)
		deleted(e, 1)
		deleted(e, 2) // 2 は検証の後に削除された（承認の対象でない）。
		id := e.queue(t, VecgenCreate, vecgenSettings, []string{vecgenLine(1), vecgenLine(2)}, func(j *db.Job) { j.Deleted, j.DeletedChoice = 1, db.DeletedInclude })
		e.s.deleted[id] = []string{imsi(1)}
		e.run(t, db.SideVecgen)
		wantCounts(t, e.s.job(id), db.JobCounts{OK: 1, Failed: 1})
		if got, _ := e.s.entry(imsi(1)); got.Deleted() || got.TenantID != 10 {
			t.Errorf("re-registered = %+v", got)
		}
		if !slices.ContainsFunc(e.s.audit, func(a db.AuditEntry) bool {
			return a.Action == audit.ActionVecgenCreate && a.Detail["reregister"] == true
		}) {
			t.Errorf("audit has no re-registration mark: %+v", e.s.audit)
		}
	})
	t.Run("除く", func(t *testing.T) {
		e := newEnv(t, 5)
		deleted(e, 1)
		id := e.queue(t, VecgenCreate, vecgenSettings, []string{vecgenLine(1), vecgenLine(3)}, func(j *db.Job) { j.Deleted, j.DeletedChoice = 1, db.DeletedExclude })
		e.s.deleted[id] = []string{imsi(1)}
		e.run(t, db.SideVecgen)
		wantCounts(t, e.s.job(id), db.JobCounts{OK: 1, Excluded: 1})
		if got, _ := e.s.entry(imsi(1)); !got.Deleted() {
			t.Errorf("excluded row was registered: %+v", got)
		}
		if rows := e.s.jobRows(id); len(rows) != 1 || rows[0].Result != db.RowExcluded {
			t.Errorf("rows = %+v", rows)
		}
	})
}

func TestUnknownResult(t *testing.T) {
	boom := pvErr(503, provisioner.CauseDownstreamUnavailable)
	t.Run("読み直して下流にあれば成功", func(t *testing.T) {
		e := newEnv(t, 1)
		e.p.fail = func(string, string) (error, bool) { return boom, true }
		id := e.queue(t, RadiusCreate, radiusSettings(), []string{imsi(1)})
		e.run(t, db.SideRadius)
		wantCounts(t, e.s.job(id), db.JobCounts{OK: 1})
		if got, _ := e.s.entry(imsi(1)); !got.RadiusPresent || got.CheckNeeded {
			t.Errorf("ledger = %+v", got)
		}
		if len(e.s.events) != 1 || e.s.events[0].Event != db.EventReconciled || e.s.events[0].JobID != "1" {
			t.Errorf("events = %+v", e.s.events)
		}
	})
	t.Run("読み直して下流になければ失敗", func(t *testing.T) {
		e := newEnv(t, 1)
		e.p.fail = func(string, string) (error, bool) { return boom, false }
		id := e.queue(t, RadiusCreate, radiusSettings(), []string{imsi(1)})
		e.run(t, db.SideRadius)
		wantCounts(t, e.s.job(id), db.JobCounts{Failed: 1})
		if _, ok := e.s.entry(imsi(1)); ok {
			t.Error("ledger has a row for a registration that did not happen")
		}
		if rows := e.s.jobRows(id); len(rows) != 1 || !rows[0].Unknown {
			t.Errorf("rows = %+v", rows)
		}
	})
	t.Run("読み直せなければ失敗で印が残る", func(t *testing.T) {
		e := newEnv(t, 1)
		e.p.fail = func(string, string) (error, bool) { return boom, false }
		e.p.batchErr = boom
		id := e.queue(t, VecgenCreate, vecgenSettings, []string{vecgenLine(1)})
		e.run(t, db.SideVecgen)
		wantCounts(t, e.s.job(id), db.JobCounts{Failed: 1})
		if got, ok := e.s.entry(imsi(1)); !ok || !got.CheckNeeded || got.TenantID != 10 {
			t.Errorf("ledger = %+v, %v", got, ok)
		}
		if rows := e.s.jobRows(id); len(rows) != 1 || !rows[0].Unknown || !strings.Contains(rows[0].Reason, "印") {
			t.Errorf("rows = %+v", rows)
		}
	})
	t.Run("削除で読み直してなくなっていれば成功", func(t *testing.T) {
		e := newEnv(t, 1)
		e.s.subs[imsi(1)] = db.Entry{IMSI: imsi(1), TenantID: 10, VecgenPresent: true, VecgenState: db.StateInactive}
		e.p.vecgen[imsi(1)] = db.StateInactive
		e.p.fail = func(string, string) (error, bool) { return boom, true }
		id := e.queue(t, VecgenDelete, Settings{}, []string{imsi(1)})
		e.run(t, db.SideVecgen)
		wantCounts(t, e.s.job(id), db.JobCounts{OK: 1})
		if got, _ := e.s.entry(imsi(1)); !got.Deleted() || got.CheckNeeded {
			t.Errorf("ledger = %+v", got)
		}
	})
}

func lines(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, imsi(i))
	}
	return out
}

func TestAutoInterrupt(t *testing.T) {
	e := newEnv(t, 1)
	calls := 0
	// 最初の 3 行は成功し、その後は接続の誤りが続く。
	e.p.fail = func(string, string) (error, bool) {
		calls++
		if calls <= 3 {
			return nil, false
		}
		return pvErr(503, provisioner.CauseDownstreamUnavailable), false
	}
	e.p.batchErr = pvErr(503, provisioner.CauseDownstreamUnavailable)
	id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 30))
	e.run(t, db.SideRadius)
	j := e.s.job(id)
	if j.State != db.JobInterrupted || j.Reason != reasonAuto {
		t.Errorf("state = %s (%s)", j.State, j.Reason)
	}
	wantCounts(t, j, db.JobCounts{OK: 3, Failed: maxConnFailure, Unprocessed: 30 - 3 - maxConnFailure})
	var unprocessed int
	for _, r := range e.s.jobRows(id) {
		if r.Result == db.RowUnprocessed {
			unprocessed++
		}
	}
	if unprocessed != 30-3-maxConnFailure {
		t.Errorf("unprocessed rows = %d", unprocessed)
	}
	if last := e.s.audit[len(e.s.audit)-1]; last.Action != audit.ActionJobInterrupt {
		t.Errorf("last audit = %+v", last)
	}
}

// 応答があれば続きの数を数え直す。下流を呼ばなかった行では数え直さない。
func TestConnFailureCounting(t *testing.T) {
	boom := pvErr(503, provisioner.CauseDownstreamUnavailable)
	t.Run("応答で数え直す", func(t *testing.T) {
		e := newEnv(t, 1)
		calls := 0
		e.p.fail = func(string, string) (error, bool) {
			calls++
			if calls%5 == 0 {
				return nil, false
			}
			return boom, false
		}
		e.p.batchErr = boom
		id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 25))
		e.run(t, db.SideRadius)
		if j := e.s.job(id); j.State != db.JobCompleted || j.OK != 5 || j.Failed != 20 {
			t.Errorf("job = %+v", j)
		}
	})
	t.Run("呼ばなかった行では数え直さない", func(t *testing.T) {
		e := newEnv(t, 1)
		e.p.fail = func(string, string) (error, bool) { return boom, false }
		e.p.batchErr = boom
		// 偶数の行は台帳に既にある。
		for i := 2; i <= 40; i += 2 {
			e.s.subs[imsi(i)] = db.Entry{IMSI: imsi(i), TenantID: 10, RadiusPresent: true, RadiusState: db.StateActive, RadiusSourceID: "v"}
		}
		id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 40))
		e.run(t, db.SideRadius)
		if j := e.s.job(id); j.State != db.JobInterrupted || j.Failed != maxConnFailure {
			t.Errorf("job = %+v", j)
		}
	})
}

func TestDBFailureInterrupts(t *testing.T) {
	e := newEnv(t, 1)
	e.s.subsErr = errors.New("db is down")
	id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 30))
	e.run(t, db.SideRadius)
	if j := e.s.job(id); j.State != db.JobInterrupted || j.Failed != maxConnFailure || len(e.p.callList()) != 0 {
		t.Errorf("job = %+v, calls = %v", j, e.p.callList())
	}
}

func TestConcurrencyLimit(t *testing.T) {
	e := newEnv(t, 3)
	e.p.gate = make(chan struct{})
	e.p.entered = make(chan string, 100)
	id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 20))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := e.r.RunNext(context.Background(), db.SideRadius); err != nil {
			t.Error(err)
		}
	}()
	// 同時の上限まで入るのを待ってから通す（上限を超えないことは最大の数で確かめる）。
	for range 3 {
		<-e.p.entered
	}
	close(e.p.gate)
	<-done
	if e.p.maxIn != 3 {
		t.Errorf("max in flight = %d, want 3", e.p.maxIn)
	}
	wantCounts(t, e.s.job(id), db.JobCounts{OK: 20})
}

func TestInterruptRunning(t *testing.T) {
	e := newEnv(t, 1)
	e.p.gate = make(chan struct{})
	e.p.entered = make(chan string, 100)
	id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 10))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := e.r.RunNext(context.Background(), db.SideRadius); err != nil {
			t.Error(err)
		}
	}()
	<-e.p.entered // 1 行目が処理中
	if _, err := e.r.Interrupt(t.Context(), id, "d2@example.com"); err != nil {
		t.Fatal(err)
	}
	// 中断の要求に気づくまで（定期の書き込み）待ってから、処理中の行を終わらせる。
	deadline := time.After(5 * time.Second)
	for e.s.job(id).InterruptBy == "" {
		select {
		case <-deadline:
			t.Fatal("interrupt was not noticed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(30 * time.Millisecond)
	close(e.p.gate)
	<-done
	j := e.s.job(id)
	if j.State != db.JobInterrupted || j.Reason != reasonRequested || j.Failed != 0 || j.OK+j.Unprocessed != 10 || j.Unprocessed == 0 {
		t.Errorf("job = %+v", j)
	}
	if got, ok := e.s.entry(imsi(1)); !ok || !got.RadiusPresent {
		t.Errorf("the row being processed was not finished: %+v", got)
	}
	// 中断は中断した人の名前で監査ログに残る。
	if last := e.s.audit[len(e.s.audit)-1]; last.Action != audit.ActionJobInterrupt || last.ActorEmail != "d2@example.com" {
		t.Errorf("last audit = %+v", last)
	}
	// もう一度は断る。
	if _, err := e.r.Interrupt(t.Context(), id, "d@example.com"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("second interrupt = %v", err)
	}
}

func TestInterruptQueued(t *testing.T) {
	e := newEnv(t, 1)
	id := e.queue(t, VecgenDelete, Settings{}, lines(1, 3))
	j, err := e.r.Interrupt(t.Context(), id, "d@example.com")
	if err != nil || j.State != db.JobInterrupted {
		t.Fatalf("interrupt = %+v, %v", j, err)
	}
	rows := e.s.jobRows(id)
	if len(rows) != 3 || rows[0].Result != db.RowUnprocessed || rows[2].Line != 4 {
		t.Errorf("rows = %+v", rows)
	}
	if j := e.s.job(id); j.Unprocessed != 3 {
		t.Errorf("unprocessed = %d", j.Unprocessed)
	}
	if ran, err := e.r.RunNext(t.Context(), db.SideVecgen); ran || err != nil {
		t.Errorf("RunNext = %v, %v", ran, err)
	}
}

func TestShutdownInterrupts(t *testing.T) {
	e := newEnv(t, 1)
	e.p.gate = make(chan struct{})
	e.p.entered = make(chan string, 100)
	id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 5))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := e.r.RunNext(ctx, db.SideRadius); err != nil {
			t.Error(err)
		}
	}()
	<-e.p.entered
	cancel()
	// 処理中の行は取り消さずに終える。
	time.Sleep(10 * time.Millisecond)
	close(e.p.gate)
	<-done
	j := e.s.job(id)
	if j.State != db.JobInterrupted || j.Reason != reasonShutdown || j.OK+j.Unprocessed != 5 {
		t.Errorf("job = %+v", j)
	}
	if got, ok := e.s.entry(imsi(1)); !ok || !got.RadiusPresent {
		t.Errorf("the row being processed was not finished: %+v, %v", got, ok)
	}
}

func TestRunFailsToOpen(t *testing.T) {
	t.Run("KEK が変わった", func(t *testing.T) {
		e := newEnv(t, 1)
		id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 3))
		other, _ := kek.New(bytes.Repeat([]byte{9}, kek.Size))
		e.r.opts.KEK = other
		e.run(t, db.SideRadius)
		j := e.s.job(id)
		if j.State != db.JobInterrupted || j.Reason != reasonDecrypt || j.Unprocessed != 3 || len(e.p.callList()) != 0 {
			t.Errorf("job = %+v", j)
		}
	})
	t.Run("違うジョブの ID の暗号文", func(t *testing.T) {
		e := newEnv(t, 1)
		a := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 3))
		b := e.queue(t, RadiusCreate, radiusSettings(), lines(4, 6))
		e.s.sealed[a], e.s.sealed[b] = e.s.sealed[b], e.s.sealed[a]
		e.run(t, db.SideRadius)
		if j := e.s.job(a); j.Reason != reasonDecrypt {
			t.Errorf("job = %+v", j)
		}
	})
	t.Run("テナントが削除された", func(t *testing.T) {
		e := newEnv(t, 1)
		st := radiusSettings()
		st.TenantID = 99
		id := e.queue(t, RadiusCreate, st, lines(1, 3))
		e.run(t, db.SideRadius)
		if j := e.s.job(id); j.State != db.JobInterrupted || j.Reason != reasonTenant {
			t.Errorf("job = %+v", j)
		}
	})
}

// 検証の後に台帳が変わった行は失敗にする（X8）。
func TestLedgerChangedAtRun(t *testing.T) {
	e := newEnv(t, 1)
	e.s.subs[imsi(1)] = db.Entry{IMSI: imsi(1), TenantID: 10, CheckNeeded: true}
	id := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 1))
	e.run(t, db.SideRadius)
	wantCounts(t, e.s.job(id), db.JobCounts{Failed: 1})
	if c := e.p.callList(); len(c) != 0 {
		t.Errorf("calls = %q", c)
	}
}

func TestRecoverAndSweep(t *testing.T) {
	e := newEnv(t, 1)
	running := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 5), func(j *db.Job) { j.State, j.OK = db.JobRunning, 2 })
	queued := e.queue(t, VecgenDelete, Settings{}, lines(6, 7))
	if err := e.r.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if j := e.s.job(running); j.State != db.JobInterrupted || j.Unprocessed != 3 || j.Reason != ReasonRestart {
		t.Errorf("running job = %+v", j)
	}
	if j := e.s.job(queued); j.State != db.JobQueued {
		t.Errorf("queued job = %+v", j)
	}
	if last := e.s.audit[len(e.s.audit)-1]; last.Action != audit.ActionJobInterrupt {
		t.Errorf("last audit = %+v", last)
	}

	// 確定しない検証済みのジョブは 24 時間で取り消す（時計を差し替える。Q9）。
	fresh := e.queue(t, RadiusDelete, Settings{}, lines(8, 8), func(j *db.Job) { j.State, j.CreatedAt = db.JobValidated, time1 })
	if err := e.r.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if j := e.s.job(fresh); j.State != db.JobValidated {
		t.Errorf("job after 0h = %+v", j)
	}
	e.s.now = time1.Add(expireAfter + time.Second)
	if err := e.r.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if j := e.s.job(fresh); j.State != db.JobCanceled || j.Reason != reasonExpired {
		t.Errorf("job after 24h = %+v", j)
	}
	if last := e.s.audit[len(e.s.audit)-1]; last.Action != audit.ActionJobCancel {
		t.Errorf("last audit = %+v", last)
	}
}

// 対象ごとに 1 つのジョブだけ動かし、対象が違えば同時に動く。Run は確定を知らされて動き、ctx の終わりで戻る。
func TestRunLoop(t *testing.T) {
	e := newEnv(t, 1)
	e.p.gate = make(chan struct{})
	e.p.entered = make(chan string, 100)
	first := e.queue(t, RadiusCreate, radiusSettings(), lines(1, 1))
	second := e.queue(t, RadiusCreate, radiusSettings(), lines(2, 2))
	other := e.queue(t, VecgenDelete, Settings{}, lines(3, 3))
	e.s.subs[imsi(3)] = db.Entry{IMSI: imsi(3), TenantID: 10, VecgenPresent: true, VecgenState: db.StateInactive}
	e.p.vecgen[imsi(3)] = db.StateInactive
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.r.Run(ctx)
	}()
	got := map[string]bool{<-e.p.entered: true, <-e.p.entered: true}
	// radius は 1 つ目だけ、vecgen は別に動く。
	if !got[imsi(1)] || !got[imsi(3)] {
		t.Errorf("entered = %v", got)
	}
	if j := e.s.job(second); j.State != db.JobQueued {
		t.Errorf("second job = %+v", j)
	}
	close(e.p.gate)
	deadline := time.Now().Add(5 * time.Second)
	for e.s.job(second).State != db.JobCompleted {
		if time.Now().After(deadline) {
			t.Fatalf("second job = %+v", e.s.job(second))
		}
		e.r.Wake(db.SideRadius)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	for _, id := range []int64{first, second, other} {
		if j := e.s.job(id); j.State != db.JobCompleted {
			t.Errorf("job %d = %+v", id, j)
		}
	}
}

func TestAAD(t *testing.T) {
	if got := AAD(12); got != "aka-ossbss/bulk-job/"+strconv.Itoa(12) {
		t.Errorf("AAD = %q", got)
	}
}
