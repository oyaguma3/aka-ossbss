package web

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/reconcile"
)

// fakeDB の突き合わせの操作。

func (f *fakeDB) ReconcileRuns(_ context.Context, limit int) ([]db.ReconcileRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.runs)
	slices.Reverse(out)
	return out[:min(limit, len(out))], f.err
}

func (f *fakeDB) ReconcileRun(_ context.Context, id int64) (db.ReconcileRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return db.ReconcileRun{}, db.ErrNotFound
}

func (f *fakeDB) LatestFinishedReconcileRun(context.Context) (db.ReconcileRun, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range slices.Backward(f.runs) {
		if r.State == db.RunCompleted || r.State == db.RunHeld {
			return r, true, nil
		}
	}
	return db.ReconcileRun{}, false, nil
}

func (f *fakeDB) ReconcileFindings(_ context.Context, q db.FindingQuery) ([]db.Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Finding
	for _, x := range f.findings {
		if x.RunID == q.RunID && (q.Kind == "" || x.Kind == q.Kind) && (q.Status == "" || x.Status == q.Status) && x.ID > q.AfterID && len(out) < q.Limit {
			out = append(out, x)
		}
	}
	return out, nil
}

func (f *fakeDB) ReconcileFinding(_ context.Context, id int64) (db.Finding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.findings {
		if x.ID == id {
			return x, nil
		}
	}
	return db.Finding{}, db.ErrNotFound
}

func (f *fakeDB) CountFindings(_ context.Context, runID int64) ([]db.FindingCount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.FindingCount
	for _, x := range f.findings {
		if x.RunID != runID {
			continue
		}
		i := slices.IndexFunc(out, func(c db.FindingCount) bool { return c.Kind == x.Kind && c.Status == x.Status })
		if i < 0 {
			out = append(out, db.FindingCount{Kind: x.Kind, Status: x.Status, Count: 1})
		} else {
			out[i].Count++
		}
	}
	return out, nil
}

func (f *fakeDB) LatestScheduledRuns(context.Context) ([]db.ScheduledRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.schedules), nil
}

// fakeReconciler は突き合わせの代わり。
type fakeReconciler struct {
	d       *fakeDB
	started []string
	err     error
	// actions は「操作 IMSI 側 テナント」、actionErr は対応の誤り、status は対応の結果（空なら imported / deleted）。
	actions   []string
	actionErr error
	status    string
}

func (f *fakeReconciler) Start(_ context.Context, trigger, by, reason string) (db.ReconcileRun, error) {
	if f.err != nil {
		return db.ReconcileRun{}, f.err
	}
	f.started = append(f.started, strings.TrimSpace(trigger+" "+by+" "+reason))
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	r := db.ReconcileRun{ID: int64(len(f.d.runs) + 1), Trigger: trigger, RequestedBy: by, ApproveReason: reason, State: db.RunRunning}
	f.d.runs = append(f.d.runs, r)
	return r, nil
}

func (f *fakeReconciler) act(op string, x db.Finding, tenant int64, status string) (string, error) {
	f.actions = append(f.actions, op+" "+x.IMSI+" "+string(x.Side)+" "+strconv.FormatInt(tenant, 10))
	if f.actionErr != nil {
		return "", f.actionErr
	}
	if f.status != "" {
		return f.status, nil
	}
	return status, nil
}

func (f *fakeReconciler) Import(_ context.Context, x db.Finding, tenant int64, _ string) (string, error) {
	return f.act("import", x, tenant, db.FindingImported)
}

func (f *fakeReconciler) Delete(_ context.Context, x db.Finding, _ string) (string, error) {
	return f.act("delete", x, 0, db.FindingDeleted)
}

type reconcileEnv struct {
	*jobEnv
	rec *fakeReconciler
}

func newReconcileEnv(t *testing.T) *reconcileEnv {
	t.Helper()
	e := newJobEnv(t)
	return &reconcileEnv{jobEnv: e, rec: &fakeReconciler{d: e.db}}
}

func (e *reconcileEnv) handler(t *testing.T, email string) http.Handler {
	t.Helper()
	h, err := New(Options{Log: discard, Version: "test", DB: e.db, Identifier: auth.Dev(email), Provisioner: e.pv,
		AuditFiles: audit.NewFiles(t.TempDir(), 14), KEK: e.kek, Jobs: e.jobs, Reconciler: e.rec})
	if err != nil {
		t.Fatal(err)
	}
	return h.Routes()
}

// addRun は実行とずれを置く。ずれの ID は 通し番号。
func (e *reconcileEnv) addRun(state string, fs ...db.Finding) db.ReconcileRun {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	r := db.ReconcileRun{ID: int64(len(e.db.runs) + 1), Trigger: db.TriggerSchedule, RequestedBy: "system", State: state, StartedAt: time.Now(),
		FindingsTotal: len(fs)}
	if state != db.RunRunning {
		r.FinishedAt = time.Now()
	}
	e.db.runs = append(e.db.runs, r)
	for _, f := range fs {
		f.ID, f.RunID = int64(len(e.db.findings)+1), r.ID
		e.db.findings = append(e.db.findings, f)
	}
	return r
}

func pendingFinding(imsi string, side db.Side, tenant int64) db.Finding {
	return db.Finding{IMSI: imsi, Kind: db.FindingDownstreamOnly, Side: side, DownstreamVal: "active", TenantID: tenant, Status: db.FindingPending}
}

func TestReconcileRoutesByRole(t *testing.T) {
	e := newReconcileEnv(t)
	e.addRun(db.RunHeld, pendingFinding(imsiNew, db.SideVecgen, 10))
	type route struct{ method, path string }
	views := []route{{"GET", "/reconcile"}, {"GET", "/reconcile/1"}}
	manages := []route{{"POST", "/reconcile/run"}, {"POST", "/reconcile/1/approve"}, {"POST", "/reconcile/1/findings"}}
	check := func(email string, rs []route, want int) {
		t.Helper()
		h := e.handler(t, email)
		for _, r := range rs {
			var code int
			if r.method == "GET" {
				code = get(h, r.path).Code
			} else {
				code = post(h, r.path, form()).Code
			}
			if code != want {
				t.Errorf("%s %s %s = %d, want %d", email, r.method, r.path, code, want)
			}
		}
	}
	for _, email := range []string{"a@example.com", "b@example.com"} {
		check(email, views, http.StatusForbidden)
		check(email, manages, http.StatusForbidden)
		mustNotContain(t, get(e.handler(t, email), "/").Body.String(), `href="/reconcile"`, "reconcile-summary", "最後の実行")
	}
	check("c@example.com", views, http.StatusOK)
	check("c@example.com", manages, http.StatusForbidden)
	check("d@example.com", views, http.StatusOK)
	// C には操作を出さない。
	c := e.handler(t, "c@example.com")
	mustNotContain(t, get(c, "/reconcile").Body.String(), "今すぐ実行する")
	mustNotContain(t, get(c, "/reconcile/1").Body.String(), "このまま直す", "取り込む", `type="checkbox"`)
	if len(e.rec.started) != 0 || len(e.rec.actions) != 0 {
		t.Errorf("started = %v, actions = %v", e.rec.started, e.rec.actions)
	}
}

func TestReconcileRun(t *testing.T) {
	e := newReconcileEnv(t)
	h := e.handler(t, "d@example.com")
	if body := get(h, "/reconcile").Body.String(); !strings.Contains(body, "まだ実行していません") || !strings.Contains(body, "今すぐ実行する") {
		t.Errorf("empty list: %s", body)
	}
	w := post(h, "/reconcile/run", form())
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != "/reconcile/1" || !slices.Equal(e.rec.started, []string{"manual d@example.com"}) {
		t.Fatalf("run = %d %v %v", w.Code, w.Header(), e.rec.started)
	}
	// 動いている間: 詳細は 2 秒ごとに読み直し、一覧も定期の読み込み。
	if body := get(h, "/reconcile/1").Body.String(); !strings.Contains(body, `hx-trigger="every 2s"`) {
		t.Errorf("detail: %s", body)
	}
	if body := get(h, "/reconcile").Body.String(); !strings.Contains(body, `hx-get="/reconcile"`) {
		t.Errorf("list: %s", body)
	}
	if w := do(h, "GET", "/reconcile/1?state=running", map[string]string{"HX-Request": "true"}); strings.Contains(w.Body.String(), "<html") || !strings.Contains(w.Body.String(), `id="reconcile-progress"`) {
		t.Errorf("poll: %s", w.Body)
	}
	if w := do(h, "GET", "/reconcile/1?state=held", map[string]string{"HX-Request": "true"}); w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("poll after a state change: %v", w.Header())
	}
	// 重ねて動かせない。
	e.rec.err = reconcile.ErrRunning
	if w := post(h, "/reconcile/run", form()); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "動いています") {
		t.Errorf("second run = %d", w.Code)
	}
	for _, p := range []string{"/reconcile/999", "/reconcile/abc"} {
		if w := get(h, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
}

func TestReconcileApprove(t *testing.T) {
	e := newReconcileEnv(t)
	h := e.handler(t, "d@example.com")
	e.addRun(db.RunHeld, db.Finding{IMSI: imsiOwn, Kind: db.FindingLedgerOnly, Side: db.SideVecgen, LedgerVal: "active", TenantID: 10, Status: db.FindingHeld})
	body := get(h, "/reconcile/1").Body.String()
	for _, want := range []string{"何も直していません", "このまま直す", "保留"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q", want)
		}
	}
	if w := post(h, "/reconcile/1/approve", form()); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "承認の理由") || len(e.rec.started) != 0 {
		t.Errorf("no reason = %d", w.Code)
	}
	w := post(h, "/reconcile/1/approve", form("reason", "バックアップから戻した"))
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != "/reconcile/2" || !slices.Equal(e.rec.started, []string{"approved d@example.com バックアップから戻した"}) {
		t.Fatalf("approve = %d %v %v", w.Code, w.Header(), e.rec.started)
	}
	if last := e.db.lastAudit(); last.Action != audit.ActionReconcileApprove || last.Detail["reason"] != "バックアップから戻した" || last.ActorEmail != "d@example.com" {
		t.Errorf("audit = %+v", last)
	}
	// 新しい実行が終わった後は、古い保留を承認できない。
	e.addRun(db.RunCompleted)
	if w := post(h, "/reconcile/1/approve", form("reason", "x")); w.Code != http.StatusConflict {
		t.Errorf("old run = %d", w.Code)
	}
	if strings.Contains(get(h, "/reconcile/1").Body.String(), "このまま直す") {
		t.Error("the old run offers the approval")
	}
	// 完了の実行は承認できない。
	if w := post(h, "/reconcile/3/approve", form("reason", "x")); w.Code != http.StatusConflict {
		t.Errorf("completed run = %d", w.Code)
	}
}

func TestReconcileFindings(t *testing.T) {
	e := newReconcileEnv(t)
	h := e.handler(t, "d@example.com")
	const a, b, c = "001010000000101", "001010000000102", "001010000000103"
	e.addRun(db.RunCompleted,
		pendingFinding(a, db.SideVecgen, 10), // 1
		pendingFinding(a, db.SideRadius, 10), // 2
		pendingFinding(b, db.SideVecgen, 0),  // 3 テナントの候補なし
		db.Finding{IMSI: c, Kind: db.FindingDeletedPresent, Side: db.SideVecgen, TenantID: 20, Status: db.FindingPending},                                                // 4
		db.Finding{IMSI: imsiOwn, Kind: db.FindingStateDiff, Side: db.SideRadius, LedgerVal: "active", DownstreamVal: "inactive", TenantID: 10, Status: db.FindingFixed}, // 5
		db.Finding{IMSI: imsiOther, Kind: db.FindingRuleTenant, Side: db.SideRadius, TenantID: 20, Status: db.FindingInfo},                                               // 6
	)
	body := get(h, "/reconcile/1").Body.String()
	for _, want := range []string{a, "IMSI が台帳にない", "台帳で削除済みなのに下流にある", "直した", "報告だけ", "取り込む", "下流から削除する", "テナントA", "対応待ち（4）"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the detail", want)
		}
	}
	// 自動で直したものは選べない。
	if n := strings.Count(body, `name="finding"`); n != 4 {
		t.Errorf("checkboxes = %d", n)
	}
	// 絞り込み。
	body = get(h, "/reconcile/1?kind=state_diff").Body.String()
	mustNotContain(t, body, a)
	if !strings.Contains(body, imsiOwn) {
		t.Error("filter by kind")
	}

	// 取り込み: IMSI ごとに 1 回、テナントは候補。監査ログに残す。
	w := post(h, "/reconcile/1/findings", form("op", "import", "finding", "1", "finding", "2"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "取り込み 1 件") {
		t.Fatalf("import = %d %s", w.Code, w.Body)
	}
	if !slices.Equal(e.rec.actions, []string{"import " + a + " vecgen 10"}) {
		t.Errorf("actions = %v", e.rec.actions)
	}
	if last := e.db.lastAudit(); last.Action != audit.ActionReconcileImport || last.Target != a || last.TenantID != 10 {
		t.Errorf("audit = %+v", last)
	}
	// テナントを選べば候補より優先する。
	e.rec.actions = nil
	post(h, "/reconcile/1/findings", form("op", "import", "finding", "1", "tenant", "20"))
	if !slices.Equal(e.rec.actions, []string{"import " + a + " vecgen 20"}) {
		t.Errorf("actions = %v", e.rec.actions)
	}
	// 候補がなくテナントも選んでいなければ断る（何もしない）。
	e.rec.actions = nil
	if w := post(h, "/reconcile/1/findings", form("op", "import", "finding", "1", "finding", "3")); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "テナントの候補のない") || len(e.rec.actions) != 0 {
		t.Errorf("no tenant = %d, %v", w.Code, e.rec.actions)
	}
	// 削除済みの取り込みは理由が必須。
	if w := post(h, "/reconcile/1/findings", form("op", "import", "finding", "4")); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "理由") || len(e.rec.actions) != 0 {
		t.Errorf("no reason = %d", w.Code)
	}
	if w := post(h, "/reconcile/1/findings", form("op", "import", "finding", "4", "reason", "戻す")); w.Code != http.StatusOK || !slices.Equal(e.rec.actions, []string{"import " + c + " vecgen 20"}) {
		t.Errorf("with reason = %d, %v", w.Code, e.rec.actions)
	}
	if last := e.db.lastAudit(); last.Detail["reason"] != "戻す" {
		t.Errorf("audit = %+v", last)
	}
	// 下流から削除: 側ごと。
	e.rec.actions = nil
	w = post(h, "/reconcile/1/findings", form("op", "delete", "finding", "1", "finding", "2"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "下流からの削除 2 件") || !slices.Equal(e.rec.actions, []string{"delete " + a + " vecgen 0", "delete " + a + " radius 0"}) {
		t.Errorf("delete = %d, %v", w.Code, e.rec.actions)
	}
	if last := e.db.lastAudit(); last.Action != audit.ActionReconcileDelete || last.Detail["side"] != "radius" {
		t.Errorf("audit = %+v", last)
	}
	// 解消済み・失敗を数える。
	e.rec.status = db.FindingGone
	before := len(e.db.audit)
	if w := post(h, "/reconcile/1/findings", form("op", "delete", "finding", "1")); !strings.Contains(w.Body.String(), "解消済み 1 件") || len(e.db.audit) != before {
		t.Errorf("gone: %s", w.Body)
	}
	e.rec.status, e.rec.actionErr = "", reconcile.ErrUnreadable
	if w := post(h, "/reconcile/1/findings", form("op", "delete", "finding", "1")); !strings.Contains(w.Body.String(), "1 件は失敗しました") || !strings.Contains(w.Body.String(), "下流を読めませんでした") {
		t.Errorf("failed: %s", w.Body)
	}
	e.rec.actionErr = nil

	// 対応できないもの: 選んでいない・操作の誤り・自動で直したもの・別の実行のずれ・古い実行。
	e.rec.actions = nil
	for name, f := range map[string][]string{
		"選んでいない": {"op", "import"},
		"操作の誤り":  {"op", "x", "finding", "1"},
		"ないずれ":   {"op", "import", "finding", "999"},
		"形式の誤り":  {"op", "import", "finding", "abc"},
	} {
		if w := post(h, "/reconcile/1/findings", form(f...)); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", name, w.Code)
		}
	}
	if w := post(h, "/reconcile/1/findings", form("op", "delete", "finding", "5")); !strings.Contains(w.Body.String(), "解消済み 1 件") || len(e.rec.actions) != 0 {
		t.Errorf("fixed row = %s, %v", w.Body, e.rec.actions)
	}
	many := []string{"op", "import"}
	for range maxFindingActions + 1 {
		many = append(many, "finding", "1")
	}
	if w := post(h, "/reconcile/1/findings", form(many...)); w.Code != http.StatusBadRequest {
		t.Errorf("too many = %d", w.Code)
	}
	e.addRun(db.RunCompleted, pendingFinding(a, db.SideVecgen, 10)) // 7
	if w := post(h, "/reconcile/1/findings", form("op", "import", "finding", "1")); w.Code != http.StatusConflict {
		t.Errorf("old run = %d", w.Code)
	}
	if w := post(h, "/reconcile/2/findings", form("op", "import", "finding", "1")); w.Code != http.StatusBadRequest || len(e.rec.actions) != 0 {
		t.Errorf("finding of another run = %d", w.Code)
	}
	body = get(h, "/reconcile/1").Body.String()
	if strings.Contains(body, `name="finding"`) || !strings.Contains(body, "新しい実行があるので") {
		t.Errorf("the old run offers actions")
	}
}

func TestReconcileDetailPaging(t *testing.T) {
	e := newReconcileEnv(t)
	var fs []db.Finding
	for i := range findingsPerPage + 3 {
		fs = append(fs, db.Finding{IMSI: "0010100000" + strconv.Itoa(10000+i), Kind: db.FindingStateDiff, Side: db.SideVecgen, Status: db.FindingFixed})
	}
	e.addRun(db.RunCompleted, fs...)
	h := e.handler(t, "c@example.com")
	body := get(h, "/reconcile/1").Body.String()
	if !strings.Contains(body, "次のページ") || !strings.Contains(body, "after=50") || strings.Contains(body, fs[findingsPerPage].IMSI) {
		t.Errorf("first page")
	}
	body = get(h, "/reconcile/1?after=50").Body.String()
	if strings.Contains(body, "次のページ") || !strings.Contains(body, fs[findingsPerPage].IMSI) {
		t.Errorf("second page")
	}
	// 残した数より多く見つけたときは知らせる。
	e.db.runs[0].FindingsTotal = 20000
	if body := get(h, "/reconcile/1").Body.String(); !strings.Contains(body, "行として残したのは") {
		t.Error("no note about the cap")
	}
}

func TestReconcileDashboard(t *testing.T) {
	e := newReconcileEnv(t)
	c := e.handler(t, "c@example.com")
	if body := get(c, "/").Body.String(); !strings.Contains(body, "まだ実行していません") {
		t.Errorf("no card")
	}
	e.addRun(db.RunCompleted, pendingFinding(imsiNew, db.SideVecgen, 10), pendingFinding(imsiNew, db.SideRadius, 10))
	e.db.runs[0].FixedCount = 3
	body := get(c, "/").Body.String()
	if !strings.Contains(body, "直した数 3") || !strings.Contains(body, ">2</a>") {
		t.Errorf("card: %s", body)
	}
	// 失敗は最後の実行に、一覧は 1 つ前の完了の結果を出す。
	e.addRun(db.RunFailed)
	e.db.runs[1].Error = "下流に届きません"
	if body := get(c, "/").Body.String(); !strings.Contains(body, "下流に届きません") || !strings.Contains(body, "直した数 3") {
		t.Errorf("failed run: %s", body)
	}
	// 保留の警告。
	e.addRun(db.RunHeld)
	if body := get(c, "/").Body.String(); !strings.Contains(body, "何も直さずに保留") {
		t.Errorf("held: %s", body)
	}
	// 定期の処理の失敗。
	e.db.schedules = []db.ScheduledRun{{Name: "retention", Status: db.ScheduleFailed, Detail: "DB に届きません", StartedAt: time.Now()},
		{Name: "check-needed", Status: db.ScheduleOK, Detail: "2 件", StartedAt: time.Now()}}
	body = get(c, "/").Body.String()
	if !strings.Contains(body, "13 か月より古い記録の削除") || !strings.Contains(body, "DB に届きません") || strings.Contains(body, "結果を確かめる印の読み直し") {
		t.Errorf("schedules: %s", body)
	}
	if body := get(c, "/reconcile").Body.String(); !strings.Contains(body, "結果を確かめる印の読み直し") {
		t.Errorf("list has no schedule state")
	}
}
