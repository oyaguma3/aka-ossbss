package radsec

import (
	"slices"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// addTenant は edge-01 だけに置いたテナントを足す（反映し直しの試験で 2 件にする）。
func (e *env) addTenant(id int64, slug string, place int64) {
	e.s.tenants = append(e.s.tenants, db.TenantRadsec{TenantID: id, Slug: slug, Port: 20000 + int(id), CACertificate: "CA", Revision: 1})
	e.s.places = append(e.s.places, db.Placement{ID: place, TenantID: id, EdgeID: "edge-01", Slot: "primary"})
}

// startRollout は edge-01 の配置 places（この順）の反映し直しを作る（画面の操作の代わり）。
func (e *env) startRollout(mode string, places ...int64) *db.Rollout {
	r := db.Rollout{ID: int64(len(e.s.rollouts) + 1), EdgeID: "edge-01", Mode: mode, State: db.RolloutRunning, CreatedBy: "op@example.com"}
	for i, id := range places {
		p := e.s.place(id)
		r.Items = append(r.Items, db.RolloutItem{Seq: i + 1, PlacementID: id, TenantID: p.TenantID, State: db.RolloutItemWaiting})
	}
	e.s.rollouts = append(e.s.rollouts, r)
	return e.s.rollout(r.ID)
}

func itemStates(r *db.Rollout) []string {
	var out []string
	for _, it := range r.Items {
		out = append(out, it.State)
	}
	return out
}

// 反映し直しは 1 件ずつ進め、1 件でも成功しなければ止まって残りに送らない。続けると失敗した 1 件からやり直す（段階 6-0 の U6-8）。
func TestRolloutOneByOneAndStops(t *testing.T) {
	e := newEnv(t)
	e.addTenant(43, "beta", 3)
	e.settle()
	e.pv.finish("edge-01", 43, "ok") // beta の最初の反映（settle の中で始まっている）
	e.pass()
	e.pv.calls = nil

	r := e.startRollout(provisioner.TemplateLatest, 1, 3)
	e.pass()
	if e.pv.count("apply-latest edge-01") != 1 || itemStates(r)[0] != db.RolloutItemApplying || r.Items[0].StartedAt.IsZero() {
		t.Fatalf("first item: %v %+v calls %q", itemStates(r), r.Items[0], e.pv.calls)
	}
	// 動いている間は次へ進まず、普段の反映も送らない。
	e.pass()
	if e.pv.count("apply") != 1 {
		t.Fatalf("applied while the first item is running: %q", e.pv.calls)
	}
	// 1 件目が前の形に戻った（失敗）。止まり、2 件目に送らない。
	e.pv.finish("edge-01", 42, "rolled_back")
	e.pass()
	e.pass()
	if r.State != db.RolloutStopped || !slices.Equal(itemStates(r), []string{db.RolloutItemFailed, db.RolloutItemWaiting}) || r.StopReason == "" {
		t.Fatalf("after the failure: %s %v (%q)", r.State, itemStates(r), r.StopReason)
	}
	if e.pv.count("apply-latest") != 1 {
		t.Fatalf("sent to the rest after the failure: %q", e.pv.calls)
	}
	if !slices.ContainsFunc(e.s.audit, func(a db.AuditEntry) bool { return a.Action == "radsec.rollout.stop" }) {
		t.Error("no audit entry for the stop")
	}

	// 続ける（画面の操作の代わり）。失敗した 1 件からやり直し、成功したら次へ、全部終わったら done。
	r.State, r.StopReason, r.Items[0].State = db.RolloutRunning, "", db.RolloutItemWaiting
	e.pass()
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	if itemStates(r)[0] != db.RolloutItemOK {
		t.Fatalf("after resume: %v", itemStates(r))
	}
	e.pass()
	if itemStates(r)[1] != db.RolloutItemApplying {
		t.Fatalf("second item: %v calls %q", itemStates(r), e.pv.calls)
	}
	e.pv.finish("edge-01", 43, "ok")
	e.pass()
	e.pass()
	if r.State != db.RolloutDone || !slices.Equal(itemStates(r), []string{db.RolloutItemOK, db.RolloutItemOK}) {
		t.Fatalf("at the end: %s %v", r.State, itemStates(r))
	}
	if e.pv.count("apply-latest edge-01") != 3 {
		t.Errorf("calls %q", e.pv.calls)
	}
}

// エッジが反映し直しを断ったら（前の形がないなど）、その 1 件で止まる。止めているテナントとエッジにないテナントは飛ばす。
func TestRolloutRefusedAndSkipped(t *testing.T) {
	e := newEnv(t)
	e.addTenant(43, "beta", 3)
	e.settle()
	e.pv.finish("edge-01", 43, "ok")
	e.pass()

	e.pv.stopped["edge-01"] = true
	r := e.startRollout(provisioner.TemplatePrevious, 1)
	e.pass()
	if itemStates(r)[0] != db.RolloutItemSkipped {
		t.Fatalf("stopped tenant: %v", itemStates(r))
	}
	e.pass()
	if r.State != db.RolloutDone {
		t.Fatalf("rollout with only skipped items: %s", r.State)
	}
	e.pv.stopped["edge-01"] = false

	e.pv.errs["apply-previous edge-01"] = &provisioner.Error{Status: 409, Problem: provisioner.Problem{Cause: "NO_PREVIOUS_TEMPLATE", Detail: "前の形の記録がないので、前の形に戻せません"}}
	r = e.startRollout(provisioner.TemplatePrevious, 3, 1)
	e.pass()
	if r.State != db.RolloutStopped || !slices.Equal(itemStates(r), []string{db.RolloutItemFailed, db.RolloutItemWaiting}) {
		t.Fatalf("refused: %s %v", r.State, itemStates(r))
	}
}

// エッジが返すひな形の状態を配置に写す（画面の古いテナントの一覧に使う。段階 6-0 の U6-7）。
func TestWriteTemplateState(t *testing.T) {
	e := newEnv(t)
	e.settle()
	ft := e.pv.edges["edge-01"][42]
	ft.sum.Template = provisioner.EdgeTemplateState{Current: new(false), CanRollback: new(true),
		Recorded: &provisioner.EdgeTemplateRef{Version: 1, Image: "sha256:0123456789abcdef0123"}}
	ft.sum.ConfigCurrent = new(false)
	e.pass()
	p := e.s.place(1)
	if p.TemplateCurrent == nil || *p.TemplateCurrent || p.ConfigCurrent == nil || *p.ConfigCurrent || !p.CanRollback || p.TemplateImage != "0123456789ab" || !p.TemplateStale() {
		t.Errorf("placement = %+v", p)
	}
	if p2 := e.s.place(2); p2.TemplateCurrent != nil || p2.TemplateStale() {
		t.Errorf("edge-02 placement = %+v", p2)
	}
}
