package web

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/bulk"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/kek"
)

// fakeJobs はジョブの実行の代わり（確定と中断を記録する）。
type fakeJobs struct {
	woken       []db.Side
	interrupted []int64
	d           *fakeDB
}

func (f *fakeJobs) Wake(target db.Side) { f.woken = append(f.woken, target) }
func (f *fakeJobs) Interrupt(_ context.Context, id int64, by string) (db.Job, error) {
	f.interrupted = append(f.interrupted, id)
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	j, ok := f.d.jobs[id]
	switch {
	case !ok:
		return db.Job{}, db.ErrNotFound
	case !j.Active():
		return db.Job{}, db.ErrConflict
	}
	j.InterruptBy = by
	return *j, nil
}

// fakeDB のジョブの操作。

func (f *fakeDB) CreateJob(_ context.Context, c db.JobCreate, seal func(int64) ([]byte, error)) (db.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jobs == nil {
		f.jobs, f.jobDeleted, f.jobSealed = map[int64]*db.Job{}, map[int64][]string{}, map[int64][]byte{}
	}
	id := int64(len(f.jobs) + 1)
	sealed, err := seal(id)
	if err != nil {
		return db.Job{}, err
	}
	j := &db.Job{ID: id, Kind: c.Kind, Target: bulk.Kind(c.Kind).Side(), State: db.JobValidated, Settings: c.Settings, FileName: c.FileName, HasFile: true,
		KEKID: c.KEKID, Total: c.Total, PlanExists: c.PlanExists, PlanAbsent: c.PlanAbsent, Deleted: len(c.DeletedIMSIs), CreatedBy: c.CreatedBy, CreatedAt: time.Now()}
	f.jobs[id], f.jobDeleted[id], f.jobSealed[id] = j, c.DeletedIMSIs, sealed
	return *j, nil
}

func (f *fakeDB) Job(_ context.Context, id int64) (db.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		return *j, nil
	}
	return db.Job{}, db.ErrNotFound
}

func (f *fakeDB) ListJobs(context.Context, int) ([]db.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Job
	for _, id := range slices.Sorted(maps.Keys(f.jobs)) {
		out = append(out, *f.jobs[id])
	}
	return out, f.err
}

func (f *fakeDB) ActiveJobs(context.Context) ([]db.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Job
	for _, id := range slices.Sorted(maps.Keys(f.jobs)) {
		if j := f.jobs[id]; j.Active() {
			out = append(out, *j)
		}
	}
	return out, f.err
}

func (f *fakeDB) JobDeletedIMSIs(_ context.Context, id int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobDeleted[id], nil
}

func (f *fakeDB) moveJob(id int64, from string, fn func(*db.Job)) (db.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return db.Job{}, db.ErrNotFound
	}
	if j.State != from {
		return db.Job{}, db.ErrConflict
	}
	fn(j)
	return *j, nil
}

func (f *fakeDB) ConfirmJob(_ context.Context, id int64, choice, reason string) (db.Job, error) {
	return f.moveJob(id, db.JobValidated, func(j *db.Job) { j.State, j.DeletedChoice, j.DeletedReason = db.JobQueued, choice, reason })
}

func (f *fakeDB) CancelJob(_ context.Context, id int64, reason string) (db.Job, error) {
	return f.moveJob(id, db.JobValidated, func(j *db.Job) { j.State, j.Reason, j.HasFile = db.JobCanceled, reason, false })
}

func (f *fakeDB) JobRows(_ context.Context, id int64, after, limit int) ([]db.JobRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.JobRow
	for _, r := range f.jobRows[id] {
		if r.Line > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeDB) SubscribersByIMSI(_ context.Context, imsis []string) (map[string]db.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]db.Entry{}
	for _, imsi := range imsis {
		if e, ok := f.subs[imsi]; ok {
			out[imsi] = e
		}
	}
	return out, f.err
}

// jobEnv はジョブの画面の試験の環境。
type jobEnv struct {
	*testEnv
	kek  *kek.KEK
	jobs *fakeJobs
}

func newJobEnv(t *testing.T) *jobEnv {
	t.Helper()
	k, err := kek.New(bytes.Repeat([]byte{5}, kek.Size))
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEnv()
	return &jobEnv{testEnv: e, kek: k, jobs: &fakeJobs{d: e.db}}
}

func (e *jobEnv) handler(t *testing.T, email string) http.Handler {
	t.Helper()
	if e.auditDir == "" {
		e.auditDir = t.TempDir()
	}
	h, err := New(Options{Log: discard, Version: "test", DB: e.db, Identifier: auth.Dev(email), Provisioner: e.pv,
		AuditFiles: audit.NewFiles(e.auditDir, 14), KEK: e.kek, Jobs: e.jobs})
	if err != nil {
		t.Fatal(err)
	}
	return h.Routes()
}

// upload はファイルを multipart で送る（htmx の要求として）。
func upload(t *testing.T, h http.Handler, fields [][2]string, name string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if data != nil {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://ossbss.example/jobs", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const (
	jobKi  = "0123456789abcdef0123456789abcdef"
	jobOPc = "fedcba9876543210fedcba9876543210"
)

func vecgenFields() [][2]string {
	return [][2]string{{"kind", "vecgen.create"}, {"tenant", "10"}, {"state", "active"}, {"sqn_type", "inc32"}, {"clients", "1"}}
}

func radiusFields() [][2]string {
	return [][2]string{{"kind", "radius.create"}, {"tenant", "10"}, {"state", "inactive"}, {"source", "vecgen"},
		{"nas_id", "*"}, {"ssids", "*"}, {"vlan_id", "100"}, {"session_timeout", ""}}
}

func vecgenCSV(imsis ...string) []byte {
	var b strings.Builder
	b.WriteString("imsi,ki,opc,amf,sqn\n")
	for _, imsi := range imsis {
		b.WriteString(imsi + "," + jobKi + "," + jobOPc + ",,\n")
	}
	return []byte(b.String())
}

func imsiCSV(imsis ...string) []byte { return []byte("imsi\n" + strings.Join(imsis, "\n") + "\n") }

func TestJobRoutesByRole(t *testing.T) {
	type route struct{ method, path string }
	e := newJobEnv(t)
	if w := upload(t, e.handler(t, "d@example.com"), nil, "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("no kind: %d", w.Code)
	}
	if w := upload(t, e.handler(t, "d@example.com"), [][2]string{{"kind", "vecgen.delete"}}, "a.csv", imsiCSV(imsiVecgenOnly)); w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	views := []route{{"GET", "/jobs"}, {"GET", "/jobs/1"}, {"GET", "/jobs/1/rows.csv"}, {"GET", "/jobs/summary"}}
	manages := []route{{"GET", "/jobs/new"}, {"POST", "/jobs/1/confirm"}, {"POST", "/jobs/1/cancel"}, {"POST", "/jobs/1/interrupt"}, {"POST", "/jobs/rules"}}
	check := func(email string, rs []route, want int) {
		t.Helper()
		h := e.handler(t, email)
		for _, r := range rs {
			var w *httptest.ResponseRecorder
			if r.method == "GET" {
				w = get(h, r.path)
			} else {
				w = post(h, r.path, nil)
			}
			if w.Code != want {
				t.Errorf("%s %s %s = %d, want %d", email, r.method, r.path, w.Code, want)
			}
		}
	}
	for _, email := range []string{"a@example.com", "b@example.com"} {
		check(email, views, http.StatusForbidden)
		check(email, manages, http.StatusForbidden)
		if w := upload(t, e.handler(t, email), vecgenFields(), "a.csv", vecgenCSV(imsiNew)); w.Code != http.StatusForbidden {
			t.Errorf("%s upload = %d", email, w.Code)
		}
	}
	check("c@example.com", views, http.StatusOK)
	check("c@example.com", manages, http.StatusForbidden)
	if w := upload(t, e.handler(t, "c@example.com"), vecgenFields(), "a.csv", vecgenCSV(imsiNew)); w.Code != http.StatusForbidden {
		t.Errorf("c upload = %d", w.Code)
	}
	check("d@example.com", views, http.StatusOK)
	// ナビゲーションは C・D だけ。
	if body := get(e.handler(t, "a@example.com"), "/").Body.String(); strings.Contains(body, `href="/jobs"`) {
		t.Error("A can see the job menu")
	}
	if body := get(e.handler(t, "c@example.com"), "/").Body.String(); !strings.Contains(body, `href="/jobs"`) || !strings.Contains(body, "jobs-summary") {
		t.Error("C cannot see the job menu or the dashboard card")
	}
}

func TestJobCreateVecgen(t *testing.T) {
	e := newJobEnv(t)
	h := e.handler(t, "d@example.com")
	w := upload(t, h, vecgenFields(), `..\..\dir\keys.csv`, vecgenCSV(imsiNew, imsiVecgenOnly))
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != "/jobs/1" {
		t.Fatalf("create = %d %q %s", w.Code, w.Header().Get("HX-Redirect"), w.Body)
	}
	j := e.db.jobs[1]
	if j.State != db.JobValidated || j.Total != 2 || j.PlanExists != 1 || j.FileName != "keys.csv" || j.KEKID != e.kek.ID() || j.CreatedBy != "d@example.com" {
		t.Errorf("job = %+v", j)
	}
	// ファイルは暗号化して置く（AAD はジョブの ID）。
	sealed := e.db.jobSealed[1]
	if bytes.Contains(sealed, []byte(jobKi)) {
		t.Error("the file is stored in plain text")
	}
	if p, err := e.kek.Open(sealed, bulk.AAD(1)); err != nil || !bytes.Contains(p, []byte(jobKi)) {
		t.Errorf("open = %q, %v", p, err)
	}
	var s bulk.Settings
	if err := json.Unmarshal(j.Settings, &s); err != nil || s.TenantID != 10 || s.State != "active" || s.SQNType != "inc32" || !slices.Equal(s.AllowedClientIDs, []int64{1}) {
		t.Errorf("settings = %+v, %v", s, err)
	}
	// 監査ログに中身を残さない。
	last := e.db.lastAudit()
	if last.Action != audit.ActionJobCreate || last.JobID != "1" || last.TenantID != 10 || last.Detail["file"] != "keys.csv" {
		t.Errorf("audit = %+v", last)
	}
	if strings.Contains(fmt.Sprint(last.Detail), jobKi) {
		t.Error("audit contains the key")
	}
	if len(e.pv.writes()) != 0 {
		t.Errorf("wrote downstream before the confirmation: %v", e.pv.writes())
	}
	// 確認の画面に件数。
	body := get(h, "/jobs/1").Body.String()
	for _, want := range []string{"登録する: 1 件", "既にあるので飛ばす: 1 件", "投入する", "取り消す"} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmation page has no %q", want)
		}
	}
	mustNotContain(t, body, jobKi, jobOPc)
	// C は件数を見られるが、操作のボタンはない。
	mustNotContain(t, get(e.handler(t, "c@example.com"), "/jobs/1").Body.String(), "投入する", "取り消す")

	// 確定すると待ち行列に入り、実行に知らせる。
	w = post(h, "/jobs/1/confirm", form())
	if w.Code != http.StatusOK || e.db.jobs[1].State != db.JobQueued || !slices.Equal(e.jobs.woken, []db.Side{db.SideVecgen}) {
		t.Errorf("confirm = %d, %+v, %v", w.Code, e.db.jobs[1], e.jobs.woken)
	}
	// 2 回目は何もしない。
	post(h, "/jobs/1/confirm", form())
	if len(e.jobs.woken) != 1 {
		t.Errorf("woken twice: %v", e.jobs.woken)
	}
}

func TestJobCreateRadiusAndRules(t *testing.T) {
	e := newJobEnv(t)
	h := e.handler(t, "d@example.com")
	if w := upload(t, h, radiusFields(), "a.csv", imsiCSV(imsiNew)); w.Header().Get("HX-Redirect") != "/jobs/1" {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var s bulk.Settings
	if err := json.Unmarshal(e.db.jobs[1].Settings, &s); err != nil {
		t.Fatal(err)
	}
	if s.SourceID != "vecgen" || len(s.Rules) != 1 || s.Rules[0].TenantID != 10 || s.Rules[0].VLANID != 100 || s.Rules[0].NASID != "*" {
		t.Errorf("settings = %+v", s)
	}
	// ルールの誤りは項目に出し、ジョブを作らない。
	bad := slices.Clone(radiusFields())
	bad[6] = [2]string{"vlan_id", "5000"}
	w := upload(t, h, bad, "a.csv", imsiCSV(imsiNew))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "VLAN は 1") || len(e.db.jobs) != 1 {
		t.Errorf("bad rule = %d %s", w.Code, w.Body)
	}
	// 知らない取得先・テナント・許可クライアント。
	bad = slices.Clone(radiusFields())
	bad[3] = [2]string{"source", "nope"}
	if w := upload(t, h, bad, "a.csv", imsiCSV(imsiNew)); w.Code != http.StatusBadRequest || len(e.db.jobs) != 1 {
		t.Errorf("unknown source = %d", w.Code)
	}
	bad = slices.Clone(vecgenFields())
	bad[1] = [2]string{"tenant", "99"}
	if w := upload(t, h, bad, "a.csv", vecgenCSV(imsiNew)); w.Code != http.StatusBadRequest || len(e.db.jobs) != 1 {
		t.Errorf("unknown tenant = %d", w.Code)
	}
	bad = slices.Clone(vecgenFields())
	bad[4] = [2]string{"clients", "99"}
	if w := upload(t, h, bad, "a.csv", vecgenCSV(imsiNew)); w.Code != http.StatusBadRequest || len(e.db.jobs) != 1 {
		t.Errorf("unknown client = %d", w.Code)
	}
	// ファイルがない。
	if w := upload(t, h, vecgenFields(), "", nil); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "ファイルを選んで") {
		t.Errorf("no file = %d", w.Code)
	}

	// ルールの編集はルールの部分だけを返す。
	w = post(h, "/jobs/rules", form("op", "add", "nas_id", "ap-1", "ssids", "corp", "vlan_id", "", "session_timeout", ""))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `id="job-rules"`) || !strings.Contains(body, "ap-1") || !strings.Contains(body, "ルール 2") || strings.Contains(body, "<html") {
		t.Errorf("add rule = %d %s", w.Code, body)
	}
	w = post(h, "/jobs/rules", form("op", "remove:0", "nas_id", "ap-1", "ssids", "corp", "vlan_id", "", "session_timeout", ""))
	if strings.Contains(w.Body.String(), "ap-1") {
		t.Errorf("remove rule: %s", w.Body)
	}
}

func TestJobFileErrors(t *testing.T) {
	e := newJobEnv(t)
	h := e.handler(t, "d@example.com")
	// 1 行でも誤りがあれば何も残さず、行番号つきで出す。鍵を画面に戻さない。
	data := vecgenCSV(imsiNew, "12345", imsiNew)
	w := upload(t, h, vecgenFields(), "a.csv", data)
	body := w.Body.String()
	if w.Code != http.StatusBadRequest || len(e.db.jobs) != 0 {
		t.Fatalf("errors = %d, jobs %d", w.Code, len(e.db.jobs))
	}
	for _, want := range []string{"何も投入していません", "IMSI は 15 桁", "2 行目と同じ"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in %s", want, body)
		}
	}
	mustNotContain(t, body, jobKi, jobOPc)
	if len(e.db.audit) != 0 {
		t.Errorf("audit = %+v", e.db.audit)
	}

	// 台帳との比べ: 別のテナント、Active の削除。
	w = upload(t, h, vecgenFields(), "a.csv", vecgenCSV(imsiOther))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "別のテナント") {
		t.Errorf("other tenant = %d %s", w.Code, w.Body)
	}
	w = upload(t, h, [][2]string{{"kind", "vecgen.delete"}}, "a.csv", imsiCSV(imsiOwn))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "物理削除できません") {
		t.Errorf("active delete = %d %s", w.Code, w.Body)
	}

	// 大きすぎるファイルはメモリーに置いたまま断る（TMPDIR に書けなくても動く。ディスクに書かないことの確かめ）。
	t.Setenv("TMPDIR", "/nonexistent-for-the-test")
	big := append([]byte("imsi\n"), bytes.Repeat([]byte("001010000000001\n"), (bulk.MaxFileBytes-16)/16)...)
	w = upload(t, h, [][2]string{{"kind", "vecgen.delete"}}, "a.csv", big)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "行が多すぎます") {
		t.Errorf("large file = %d %s", w.Code, w.Body.String()[:min(w.Body.Len(), 300)])
	}
	over := bytes.Repeat([]byte("a"), maxUploadBytes+1)
	w = upload(t, h, [][2]string{{"kind", "vecgen.delete"}}, "a.csv", over)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "大きすぎます") {
		t.Errorf("too large file = %d", w.Code)
	}
}

// 結果を確かめている IMSI は、その場で読み直してから判定する（読み直せなければ誤り）。
func TestJobRecheck(t *testing.T) {
	e := newJobEnv(t)
	h := e.handler(t, "d@example.com")
	marked := func() {
		e.db.put(db.Entry{IMSI: imsiNew, TenantID: 10, CheckNeeded: true})
	}
	marked()
	e.pv.errs["vecgen.batch"] = pvError(503, "DOWNSTREAM_UNAVAILABLE")
	w := upload(t, h, vecgenFields(), "a.csv", vecgenCSV(imsiNew))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "確かめている") || len(e.db.jobs) != 0 {
		t.Fatalf("marked and unreadable = %d %s", w.Code, w.Body)
	}
	delete(e.pv.errs, "vecgen.batch")
	// 下流を読めれば印を下ろして進む（どちらの側にもなければ行が消える）。
	w = upload(t, h, vecgenFields(), "a.csv", vecgenCSV(imsiNew))
	if w.Header().Get("HX-Redirect") != "/jobs/1" {
		t.Fatalf("after recheck = %d %s", w.Code, w.Body)
	}
	if _, ok := e.db.entry(imsiNew); ok {
		t.Error("pending row remains")
	}
	// 下流にあったときは既にある。
	marked()
	e.pv.addVecgen(imsiNew, "active")
	w = upload(t, h, vecgenFields(), "a.csv", vecgenCSV(imsiNew))
	if w.Header().Get("HX-Redirect") != "/jobs/2" || e.db.jobs[2].PlanExists != 1 {
		t.Errorf("recheck found downstream = %d, %+v", w.Code, e.db.jobs[2])
	}
}

func TestJobDeletedChoice(t *testing.T) {
	e := newJobEnv(t)
	h := e.handler(t, "d@example.com")
	if w := upload(t, h, vecgenFields(), "a.csv", vecgenCSV(imsiDeleted, imsiNew)); w.Header().Get("HX-Redirect") != "/jobs/1" {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	body := get(h, "/jobs/1").Body.String()
	for _, want := range []string{"削除済みの IMSI が 1 件", imsiDeleted, "承認して含める", "除いて進める", "取り消す"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q", want)
		}
	}
	// 選ばない・理由がないと進まない。
	if w := post(h, "/jobs/1/confirm", form()); w.Code != http.StatusBadRequest || e.db.jobs[1].State != db.JobValidated {
		t.Errorf("no choice = %d", w.Code)
	}
	if w := post(h, "/jobs/1/confirm", form("choice", "include")); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "承認の理由") {
		t.Errorf("no reason = %d", w.Code)
	}
	if w := post(h, "/jobs/1/confirm", form("choice", "include", "reason", "戻す回線")); w.Code != http.StatusOK {
		t.Fatalf("include = %d %s", w.Code, w.Body)
	}
	j := e.db.jobs[1]
	if j.State != db.JobQueued || j.DeletedChoice != db.DeletedInclude || j.DeletedReason != "戻す回線" {
		t.Errorf("job = %+v", j)
	}
	last := e.db.lastAudit()
	if last.Action != audit.ActionJobReregister || last.Detail["count"] != 1 || last.Detail["reason"] != "戻す回線" {
		t.Errorf("audit = %+v", last)
	}
	// 除く。承認の監査ログは書かない。
	if w := upload(t, h, vecgenFields(), "a.csv", vecgenCSV(imsiDeleted)); w.Header().Get("HX-Redirect") != "/jobs/2" {
		t.Fatalf("create = %d", w.Code)
	}
	before := len(e.db.audit)
	if w := post(h, "/jobs/2/confirm", form("choice", "exclude", "reason", "無視される")); w.Code != http.StatusOK {
		t.Fatalf("exclude = %d", w.Code)
	}
	if j := e.db.jobs[2]; j.DeletedChoice != db.DeletedExclude || j.DeletedReason != "" || len(e.db.audit) != before {
		t.Errorf("job = %+v, audit %d → %d", j, before, len(e.db.audit))
	}
	// 取り消し。
	if w := upload(t, h, vecgenFields(), "a.csv", vecgenCSV(imsiDeleted)); w.Header().Get("HX-Redirect") != "/jobs/3" {
		t.Fatalf("create = %d", w.Code)
	}
	if w := post(h, "/jobs/3/cancel", form()); w.Code != http.StatusOK {
		t.Fatalf("cancel = %d", w.Code)
	}
	if j := e.db.jobs[3]; j.State != db.JobCanceled || j.HasFile {
		t.Errorf("job = %+v", j)
	}
	if last := e.db.lastAudit(); last.Action != audit.ActionJobCancel {
		t.Errorf("audit = %+v", last)
	}
	// 確定した後は取り消せない。
	if w := post(h, "/jobs/1/cancel", form()); w.Code != http.StatusOK || e.db.jobs[1].State != db.JobQueued {
		t.Errorf("cancel of a queued job = %d, %+v", w.Code, e.db.jobs[1])
	}
}

func TestJobDetailAndInterrupt(t *testing.T) {
	e := newJobEnv(t)
	h := e.handler(t, "d@example.com")
	if w := upload(t, h, [][2]string{{"kind", "radius.delete"}}, "a.csv", imsiCSV(imsiVecgenOnly)); w.Code != http.StatusOK {
		t.Fatalf("create = %d", w.Code)
	}
	post(h, "/jobs/1/confirm", form())
	e.db.mu.Lock()
	j := e.db.jobs[1]
	j.State, j.Total, j.OK, j.Failed, j.Exists = db.JobRunning, 10, 3, 1, 1
	e.db.jobRows = map[int64][]db.JobRow{1: {
		{Line: 3, IMSI: imsiOwn, Result: db.RowFailed, Reason: "接続できません, 詳しくは", Unknown: true},
		{Line: 5, IMSI: imsiOther, Result: db.RowExists},
	}}
	e.db.mu.Unlock()

	body := get(h, "/jobs/1").Body.String()
	for _, want := range []string{"5 / 10", "中断する", "結果不明", "接続できません", `hx-trigger="every 2s"`, "rows.csv"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the detail", want)
		}
	}
	mustNotContain(t, get(e.handler(t, "c@example.com"), "/jobs/1").Body.String(), "中断する")

	// 定期の読み込みは進み具合の部分だけ。状態が変わったら画面ごと読み直させる。
	w := do(h, "GET", "/jobs/1?state=running", map[string]string{"HX-Request": "true"})
	if b := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(b, `id="job-progress"`) || strings.Contains(b, "<html") || strings.Contains(b, "中断する") {
		t.Errorf("poll = %d %s", w.Code, b)
	}
	w = do(h, "GET", "/jobs/1?state=queued", map[string]string{"HX-Request": "true"})
	if w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("poll after a state change: %v", w.Header())
	}

	// CSV（鍵なし。BOM つき）。
	w = get(h, "/jobs/1/rows.csv")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "job-1-rows.csv") {
		t.Errorf("csv = %d %v", w.Code, w.Header())
	}
	want := "\xEF\xBB\xBFline,imsi,result,unknown,reason\n3," + imsiOwn + ",失敗,結果不明,\"接続できません, 詳しくは\"\n5," + imsiOther + ",既にある,,\n"
	if got := w.Body.String(); got != want {
		t.Errorf("csv = %q, want %q", got, want)
	}

	// 中断。
	w = post(h, "/jobs/1/interrupt", form())
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("HX-Redirect"), "interrupted=1") || !slices.Equal(e.jobs.interrupted, []int64{1}) {
		t.Errorf("interrupt = %d %v %v", w.Code, w.Header(), e.jobs.interrupted)
	}
	if body := get(h, "/jobs/1?interrupted=1").Body.String(); !strings.Contains(body, "中断を受け付けました") {
		t.Errorf("no message: %s", body)
	}
	// 終わったジョブは中断できない（画面に戻るだけ）。
	e.db.mu.Lock()
	j.State = db.JobCompleted
	e.db.mu.Unlock()
	if w := post(h, "/jobs/1/interrupt", form()); w.Code != http.StatusOK || strings.Contains(w.Header().Get("HX-Redirect"), "interrupted") {
		t.Errorf("interrupt of a finished job = %d %v", w.Code, w.Header())
	}
	// ない・形式の違うジョブ。
	for _, p := range []string{"/jobs/999", "/jobs/abc", "/jobs/0"} {
		if w := get(h, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
}

func TestJobListAndSummary(t *testing.T) {
	e := newJobEnv(t)
	h := e.handler(t, "c@example.com")
	if body := get(h, "/jobs").Body.String(); !strings.Contains(body, "ジョブはありません") || strings.Contains(body, "ジョブを登録") {
		t.Errorf("empty list: %s", body)
	}
	if body := get(h, "/").Body.String(); !strings.Contains(body, "待ちと実行中のジョブはありません") {
		t.Error("dashboard has no job card")
	}
	d := e.handler(t, "d@example.com")
	if body := get(d, "/jobs").Body.String(); !strings.Contains(body, "ジョブを登録") {
		t.Error("D has no register button")
	}
	if w := upload(t, d, [][2]string{{"kind", "radius.delete"}}, "a.csv", imsiCSV(imsiVecgenOnly)); w.Code != http.StatusOK {
		t.Fatalf("create = %d", w.Code)
	}
	post(d, "/jobs/1/confirm", form())
	body := get(h, "/jobs").Body.String()
	if !strings.Contains(body, "radius 側からの物理削除") || !strings.Contains(body, `hx-get="/jobs"`) {
		t.Errorf("list: %s", body)
	}
	w := get(h, "/jobs/summary")
	if b := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(b, "#1") || !strings.Contains(b, "待ち") || strings.Contains(b, "<html") {
		t.Errorf("summary = %d %s", w.Code, b)
	}
	// 3 つの画面の種類のタブと、ヘッダーの案内。
	body = get(d, "/jobs/new?kind=vecgen.create").Body.String()
	for _, want := range []string{"imsi,ki,opc,amf,sqn", "許可クライアント", "radius-av", `hx-encoding="multipart/form-data"`} {
		if !strings.Contains(body, want) {
			t.Errorf("new form has no %q", want)
		}
	}
	body = get(d, "/jobs/new?kind=radius.create").Body.String()
	for _, want := range []string{"取得先", "ルール 1", "ルールを追加"} {
		if !strings.Contains(body, want) {
			t.Errorf("new form has no %q", want)
		}
	}
	mustNotContain(t, get(d, "/jobs/new?kind=radius.delete").Body.String(), "所属のテナント", "ルールを追加")
}
