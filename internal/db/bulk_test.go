package db

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/kek"
)

func TestJobLifecycle(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	k, err := kek.New(bytes.Repeat([]byte{1}, kek.Size))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := kek.New(bytes.Repeat([]byte{2}, kek.Size))
	plain := []byte("imsi\n001010000000001\n")
	aad := func(id int64) string { return "aka-ossbss/bulk-job/" + strconv.FormatInt(id, 10) }
	create := func(kind string, deleted ...string) Job {
		t.Helper()
		j, err := d.CreateJob(ctx, JobCreate{Kind: kind, Settings: []byte(`{"tenantId":1}`), FileName: "a.csv", KEKID: k.ID(), Total: 3,
			PlanExists: 1, PlanAbsent: 1, DeletedIMSIs: deleted, CreatedBy: "d@example.com"},
			func(id int64) ([]byte, error) { return k.Seal(plain, aad(id)) })
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	j := create("vecgen.create", "001010000000009", "001010000000008")
	if j.State != JobValidated || j.Target != SideVecgen || !j.HasFile || j.Deleted != 2 || j.KEKID != k.ID() {
		t.Fatalf("created = %+v", j)
	}
	if got, err := d.JobDeletedIMSIs(ctx, j.ID); err != nil || !slices.Equal(got, []string{"001010000000008", "001010000000009"}) {
		t.Errorf("deleted imsis = %v, %v", got, err)
	}
	if _, err := d.Job(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
	// まだ確定していないので待ち行列にない。実行もできない。
	if _, ok, err := d.NextQueuedJob(ctx, SideVecgen); ok || err != nil {
		t.Errorf("next before confirm = %v, %v", ok, err)
	}
	if _, err := d.StartJob(ctx, j.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("start before confirm = %v", err)
	}

	j, err = d.ConfirmJob(ctx, j.ID, DeletedInclude, "戻す回線")
	if err != nil || j.State != JobQueued || j.QueuedAt.IsZero() || j.DeletedChoice != DeletedInclude || j.DeletedReason != "戻す回線" {
		t.Fatalf("confirm = %+v, %v", j, err)
	}
	if _, err := d.ConfirmJob(ctx, j.ID, "", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("confirm twice = %v", err)
	}
	if _, err := d.CancelJob(ctx, j.ID, "x"); !errors.Is(err, ErrConflict) {
		t.Errorf("cancel of a queued job = %v", err)
	}

	// 待ち行列は対象ごとに、確定した順。
	j2 := create("radius.create")
	j3 := create("vecgen.delete")
	if _, err := d.ConfirmJob(ctx, j3.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmJob(ctx, j2.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := d.NextQueuedJob(ctx, SideVecgen); !ok || err != nil || n.ID != j.ID {
		t.Errorf("next vecgen = %+v, %v, %v", n, ok, err)
	}
	if n, ok, err := d.NextQueuedJob(ctx, SideRadius); !ok || err != nil || n.ID != j2.ID {
		t.Errorf("next radius = %+v, %v, %v", n, ok, err)
	}

	// 実行中にすると暗号化したファイルが返り、復号できる。違う KEK・違うジョブの ID では復号できない。
	started, err := d.StartJob(ctx, j.ID)
	if err != nil || started.State != JobRunning || started.StartedAt.IsZero() {
		t.Fatalf("start = %+v, %v", started, err)
	}
	if p, err := k.Open(started.Sealed, aad(j.ID)); err != nil || !bytes.Equal(p, plain) {
		t.Errorf("open = %q, %v", p, err)
	}
	if _, err := other.Open(started.Sealed, aad(j.ID)); !errors.Is(err, kek.ErrDecrypt) {
		t.Errorf("open with another KEK = %v", err)
	}
	if _, err := k.Open(started.Sealed, aad(j.ID+1)); !errors.Is(err, kek.ErrDecrypt) {
		t.Errorf("open with another job id = %v", err)
	}

	// 進み具合と成功以外の行。
	by, err := d.FlushJobProgress(ctx, j.ID, JobCounts{OK: 1, Exists: 1, Failed: 1}, []JobRow{
		{Line: 2, IMSI: "001010000000001", Result: RowExists},
		{Line: 3, IMSI: "001010000000002", Result: RowFailed, Reason: "接続できません", Unknown: true},
	})
	if err != nil || by != "" {
		t.Fatalf("flush = %q, %v", by, err)
	}
	if _, err := d.RequestJobInterrupt(ctx, j.ID, "d2@example.com"); err != nil {
		t.Fatal(err)
	}
	if by, err := d.FlushJobProgress(ctx, j.ID, JobCounts{}, nil); err != nil || by != "d2@example.com" {
		t.Errorf("flush after the request = %q, %v", by, err)
	}
	rows, err := d.JobRows(ctx, j.ID, 0, 10)
	if err != nil || len(rows) != 2 || rows[1] != (JobRow{Line: 3, IMSI: "001010000000002", Result: RowFailed, Reason: "接続できません", Unknown: true}) {
		t.Errorf("rows = %+v, %v", rows, err)
	}
	if rows, err := d.JobRows(ctx, j.ID, 2, 10); err != nil || len(rows) != 1 || rows[0].Line != 3 {
		t.Errorf("rows after 2 = %+v, %v", rows, err)
	}

	// 終えるとファイルが消える。
	f, err := d.FinishJob(ctx, j.ID, JobInterrupted, "中断を求められました")
	if err != nil || f.HasFile || f.State != JobInterrupted || f.FinishedAt.IsZero() || f.OK != 1 || f.Failed != 1 {
		t.Fatalf("finish = %+v, %v", f, err)
	}
	if _, err := d.RequestJobInterrupt(ctx, j.ID, "x@example.com"); !errors.Is(err, ErrConflict) {
		t.Errorf("request after finish = %v", err)
	}

	// 待ちのジョブの中断（暗号化していたファイルを返し、消す）。
	took, err := d.TakeQueuedJob(ctx, j3.ID, "d@example.com", "やめる")
	if err != nil || took.HasFile || took.State != JobInterrupted || len(took.Sealed) == 0 || took.Unprocessed != took.Total || took.InterruptBy != "d@example.com" {
		t.Fatalf("take = %+v, %v", took, err)
	}

	// 起動のとき: 実行中は中断（未処理は残りの数）、待ちはそのまま。
	if _, err := d.StartJob(ctx, j2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.FlushJobProgress(ctx, j2.ID, JobCounts{OK: 1}, nil); err != nil {
		t.Fatal(err)
	}
	j4 := create("radius.delete")
	if _, err := d.ConfirmJob(ctx, j4.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	active, err := d.ActiveJobs(ctx)
	if err != nil || len(active) != 2 || active[0].ID != j2.ID || active[1].ID != j4.ID {
		t.Errorf("active = %+v, %v", active, err)
	}
	recovered, err := d.InterruptRunningJobs(ctx, "止まりました")
	if err != nil || len(recovered) != 1 || recovered[0].ID != j2.ID || recovered[0].Unprocessed != 2 || recovered[0].HasFile || recovered[0].Reason != "止まりました" {
		t.Fatalf("recovered = %+v, %v", recovered, err)
	}
	if n, ok, err := d.NextQueuedJob(ctx, SideRadius); !ok || err != nil || n.ID != j4.ID {
		t.Errorf("queued job after the restart = %+v, %v, %v", n, ok, err)
	}

	// 確定しない検証済みのジョブは取り消す（時刻は呼び出し側が決める）。
	j5 := create("vecgen.delete")
	if exp, err := d.ExpireValidatedJobs(ctx, time.Now().Add(-time.Hour), "古い"); err != nil || len(exp) != 0 {
		t.Errorf("expire of a new job = %+v, %v", exp, err)
	}
	exp, err := d.ExpireValidatedJobs(ctx, time.Now().Add(time.Hour), "古い")
	if err != nil || len(exp) != 1 || exp[0].ID != j5.ID || exp[0].State != JobCanceled || exp[0].HasFile {
		t.Errorf("expire = %+v, %v", exp, err)
	}
	j6 := create("radius.delete")
	if c, err := d.CancelJob(ctx, j6.ID, "取り消し"); err != nil || c.State != JobCanceled || c.HasFile {
		t.Errorf("cancel = %+v, %v", c, err)
	}

	// 一覧は開いているものが先。
	list, err := d.ListJobs(ctx, 3)
	if err != nil || len(list) != 3 || list[0].ID != j4.ID {
		t.Errorf("list = %+v, %v", list, err)
	}
}

func TestJobLock(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	release, ok, err := d.TryJobLock(ctx, SideVecgen)
	if err != nil || !ok {
		t.Fatalf("first lock = %v, %v", ok, err)
	}
	if _, ok, err := d.TryJobLock(ctx, SideVecgen); ok || err != nil {
		t.Errorf("second lock = %v, %v", ok, err)
	}
	// 対象が違えば別のロック。
	r2, ok, err := d.TryJobLock(ctx, SideRadius)
	if err != nil || !ok {
		t.Fatalf("radius lock = %v, %v", ok, err)
	}
	r2()
	release()
	r3, ok, err := d.TryJobLock(ctx, SideVecgen)
	if err != nil || !ok {
		t.Fatalf("lock after release = %v, %v", ok, err)
	}
	r3()
}

func TestSubscribersByIMSI(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := d.CreateTenant(ctx, "テナントA", Meta{Operator: "d@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	register(t, d, "001010000000001", a.ID)
	got, err := d.SubscribersByIMSI(ctx, []string{"001010000000001", "001010000000002"})
	if err != nil || len(got) != 1 || got["001010000000001"].TenantID != a.ID {
		t.Errorf("got %+v, %v", got, err)
	}
}
