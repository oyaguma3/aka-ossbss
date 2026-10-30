package db

import (
	"errors"
	"slices"
	"testing"
)

func TestReconcileRuns(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.LatestFinishedReconcileRun(ctx); ok || err != nil {
		t.Errorf("latest of nothing = %v, %v", ok, err)
	}
	r, err := d.CreateReconcileRun(ctx, TriggerManual, "d@example.com", "")
	if err != nil || r.State != RunRunning || r.Trigger != TriggerManual || r.RequestedBy != "d@example.com" || r.StartedAt.IsZero() {
		t.Fatalf("create = %+v, %v", r, err)
	}
	// 実行中は「最新」に数えない。
	if _, ok, _ := d.LatestFinishedReconcileRun(ctx); ok {
		t.Error("a running run was counted as the latest")
	}
	if err := d.UpdateReconcileProgress(ctx, r.ID, 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ReconcileRun(ctx, r.ID); err != nil || got.LedgerRows != 1 || got.VecgenRows != 2 || got.RadiusRows != 3 {
		t.Errorf("progress = %+v, %v", got, err)
	}
	r.State, r.LedgerRows, r.VecgenRows, r.RadiusRows, r.FixableCount, r.FixedCount, r.FindingsTotal, r.Error = RunCompleted, 5, 6, 7, 2, 1, 3, ""
	done, err := d.FinishReconcileRun(ctx, r)
	if err != nil || done.State != RunCompleted || done.FixedCount != 1 || done.FindingsTotal != 3 || done.FinishedAt.IsZero() {
		t.Errorf("finish = %+v, %v", done, err)
	}
	if got, ok, err := d.LatestFinishedReconcileRun(ctx); !ok || err != nil || got.ID != r.ID {
		t.Errorf("latest = %+v, %v, %v", got, ok, err)
	}
	if _, err := d.ReconcileRun(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}

	r2, err := d.CreateReconcileRun(ctx, TriggerSchedule, "system", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.InterruptRunningReconciles(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.ReconcileRun(ctx, r2.ID); got.State != RunInterrupted || got.FinishedAt.IsZero() {
		t.Errorf("interrupted = %+v", got)
	}
	if got, ok, _ := d.LatestFinishedReconcileRun(ctx); !ok || got.ID != r.ID {
		t.Errorf("latest after an interrupted run = %+v", got)
	}
	list, err := d.ReconcileRuns(ctx, 10)
	if err != nil || len(list) != 2 || list[0].ID != r2.ID {
		t.Errorf("list = %+v, %v", list, err)
	}

	// ずれ。
	const a, b = "001010000000001", "001010000000002"
	if err := d.AddFindings(ctx, r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.AddFindings(ctx, r.ID, []Finding{
		{IMSI: a, Kind: FindingDownstreamOnly, Side: SideVecgen, DownstreamVal: "active", TenantID: 10, Status: FindingPending},
		{IMSI: a, Kind: FindingDownstreamOnly, Side: SideRadius, DownstreamVal: "active", TenantID: 10, Status: FindingPending},
		{IMSI: b, Kind: FindingStateDiff, Side: SideRadius, LedgerVal: "active", DownstreamVal: "inactive", Status: FindingFixed},
	}); err != nil {
		t.Fatal(err)
	}
	all, err := d.ReconcileFindings(ctx, FindingQuery{RunID: r.ID, Limit: 10})
	if err != nil || len(all) != 3 || all[0].TenantID != 10 || all[0].Side != SideVecgen || all[2].LedgerVal != "active" {
		t.Fatalf("findings = %+v, %v", all, err)
	}
	if got, _ := d.ReconcileFindings(ctx, FindingQuery{RunID: r.ID, Kind: FindingStateDiff, Limit: 10}); len(got) != 1 || got[0].IMSI != b {
		t.Errorf("by kind = %+v", got)
	}
	if got, _ := d.ReconcileFindings(ctx, FindingQuery{RunID: r.ID, Status: FindingPending, AfterID: all[0].ID, Limit: 10}); len(got) != 1 || got[0].ID != all[1].ID {
		t.Errorf("by status after = %+v", got)
	}
	if got, err := d.ReconcileFinding(ctx, all[0].ID); err != nil || got.IMSI != a {
		t.Errorf("finding = %+v, %v", got, err)
	}
	if _, err := d.ReconcileFinding(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing finding = %v", err)
	}
	counts, err := d.CountFindings(ctx, r.ID)
	if err != nil || !slices.Equal(counts, []FindingCount{{FindingDownstreamOnly, FindingPending, 2}, {FindingStateDiff, FindingFixed, 1}}) {
		t.Errorf("counts = %+v, %v", counts, err)
	}
	// 側を指定すればその側だけ、指定しなければ IMSI の未対応のものすべて。
	if n, err := d.ResolveFindings(ctx, r.ID, a, SideRadius, FindingDeleted, "d@example.com"); err != nil || n != 1 {
		t.Errorf("resolve side = %d, %v", n, err)
	}
	if n, err := d.ResolveFindings(ctx, r.ID, a, "", FindingImported, "d@example.com"); err != nil || n != 1 {
		t.Errorf("resolve rest = %d, %v", n, err)
	}
	if n, _ := d.ResolveFindings(ctx, r.ID, b, "", FindingImported, "d@example.com"); n != 0 {
		t.Errorf("resolved a row that is not pending: %d", n)
	}
	if got, _ := d.ReconcileFinding(ctx, all[0].ID); got.Status != FindingImported || got.ResolvedBy != "d@example.com" || got.ResolvedAt.IsZero() {
		t.Errorf("resolved = %+v", got)
	}
}

func TestReconcileEntriesAndCheckNeeded(t *testing.T) {
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
	register(t, d, "001010000000002", a.ID)
	register(t, d, "001010000000003", a.ID)
	// 2 を削除済みにし、印を立てる。
	if err := d.WithSubscriber(ctx, "001010000000002", Meta{}, func(l Ledger) error {
		for _, s := range Sides {
			if e, _ := l.Entry(); e.Present(s) {
				if err := l.Apply(Change{Kind: ChangeDelete, Side: s}); err != nil {
					return err
				}
			}
		}
		return l.Apply(Change{Kind: ChangeCheckNeeded})
	}); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReconcileEntries(ctx, "", 2)
	if err != nil || len(got) != 2 || got[1].IMSI != "001010000000002" || !got[1].Deleted() {
		t.Errorf("entries = %+v, %v", got, err)
	}
	got, err = d.ReconcileEntries(ctx, got[1].IMSI, 2)
	if err != nil || len(got) != 1 || got[0].IMSI != "001010000000003" {
		t.Errorf("next entries = %+v, %v", got, err)
	}
	if imsis, err := d.CheckNeededIMSIs(ctx, 10); err != nil || !slices.Equal(imsis, []string{"001010000000002"}) {
		t.Errorf("check needed = %v, %v", imsis, err)
	}
}
