package web

import (
	"context"
	"encoding/json/jsontext"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// fakePV はテスト用の aka-provisioner。両側の加入者をメモリに持ち、書き込みごとに
// 「口 操作者 Idempotency-Key」を記録する（本文の秘密の値は記録しない）。
type fakePV struct {
	mu sync.Mutex
	// status と statusErr は Status が返すもの。statusCalls はその呼び出し回数。
	status      provisioner.Status
	statusErr   error
	statusCalls int

	vecgen   map[string]provisioner.VecgenSubscriber
	radius   map[string]provisioner.RadiusSubscriber
	clients  []provisioner.VecgenClient
	sources  []provisioner.Source
	sessions []provisioner.Session
	audit    map[provisioner.AuditSource][]provisioner.AuditLogEntry
	// errs は口ごとに返す誤り（"vecgen.create" など）。applyErrs は、下流には反映したうえで返す誤り（結果が分からない場合）。
	errs, applyErrs map[string]error
	// ops は書き込みの記録。lastSessions は最後のセッションの条件、lastRadiusUpdate は最後の radius 側の変更、lastVecgenCreate は
	// 最後の vecgen 側の登録（Ki・OPc を含む。送ったことの確かめにだけ使う）。
	ops              []string
	lastSessions     provisioner.SessionParams
	lastRadiusUpdate provisioner.RadiusSubscriberUpdate
	lastVecgenUpdate provisioner.VecgenSubscriberUpdate
	lastVecgenCreate provisioner.VecgenSubscriberCreate
	batchCalls       int
}

func newFakePV() *fakePV {
	vecgenStatus := jsontext.Value(`{"version":"0.5.0","subscriberCount":3,"avApiContractVersion":"1.0.0"}`)
	return &fakePV{
		status: provisioner.Status{Version: "2.0.0", StartedAt: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC),
			Downstreams: []provisioner.DownstreamStatus{
				{Name: provisioner.Vecgen, Reachable: true, Status: vecgenStatus, CheckedAt: time.Now()},
				{Name: provisioner.Radius, Reachable: false, Hint: "aka-radius に届きません。"},
			}},
		vecgen: map[string]provisioner.VecgenSubscriber{}, radius: map[string]provisioner.RadiusSubscriber{},
		clients: []provisioner.VecgenClient{{ID: 1, Name: "radius-av", Enabled: true}, {ID: 2, Name: "other", Enabled: true}},
		sources: []provisioner.Source{{ID: "vecgen", Name: "aka-vecgen"}, {ID: "vecgen-2", Name: "aka-vecgen-2"}},
		audit:   map[provisioner.AuditSource][]provisioner.AuditLogEntry{},
		errs:    map[string]error{}, applyErrs: map[string]error{},
	}
}

// addVecgen と addRadius は下流に加入者を置く（試験の準備）。
func (f *fakePV) addVecgen(imsi, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vecgen[imsi] = provisioner.VecgenSubscriber{IMSI: imsi, State: state, SQN: "000000000020", AMF: "8000", SQNType: "inc32",
		AllowedClientIDs: []int64{1}}
}

func (f *fakePV) addRadius(imsi, state string, tenant int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.radius[imsi] = provisioner.RadiusSubscriber{IMSI: imsi, State: state, SourceID: "vecgen",
		Rules: []provisioner.Rule{{TenantID: tenant, NASID: "ap-01", SSIDs: []string{"corp"}, VLANID: 10}}}
}

// record は書き込みを記録し、その口の誤りを返す（applyErrs のときは、反映してから誤りを返すよう呼び出し側で扱う）。
func (f *fakePV) record(ctx context.Context, op, key string) error {
	f.ops = append(f.ops, strings.TrimSpace(op+" "+provisioner.OperatorFrom(ctx)+" "+key))
	return f.errs[op]
}

// lastOp は最後の書き込みの記録を返す。
func (f *fakePV) lastOp() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ops) == 0 {
		return ""
	}
	return f.ops[len(f.ops)-1]
}

func (f *fakePV) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops)
}

func notFound() error {
	return &provisioner.Error{Status: 404, Problem: provisioner.Problem{Status: 404, Cause: provisioner.CauseUserNotFound}}
}

func pvError(status int, cause string) error {
	return &provisioner.Error{Status: status, Problem: provisioner.Problem{Status: status, Cause: cause}}
}

func (f *fakePV) Status(context.Context) (provisioner.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	return f.status, f.statusErr
}

func (f *fakePV) VecgenSubscriber(_ context.Context, imsi string) (provisioner.VecgenSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["vecgen.get"]; err != nil {
		return provisioner.VecgenSubscriber{}, err
	}
	s, ok := f.vecgen[imsi]
	if !ok {
		return s, notFound()
	}
	return s, nil
}

func (f *fakePV) VecgenBatchGet(_ context.Context, imsis []string) (provisioner.VecgenBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	if err := f.errs["vecgen.batch"]; err != nil {
		return provisioner.VecgenBatch{}, err
	}
	var b provisioner.VecgenBatch
	for _, imsi := range imsis {
		if s, ok := f.vecgen[imsi]; ok {
			b.Items = append(b.Items, s)
		} else {
			b.NotFound = append(b.NotFound, imsi)
		}
	}
	return b, nil
}

func (f *fakePV) CreateVecgenSubscriber(ctx context.Context, s provisioner.VecgenSubscriberCreate, key string) (provisioner.VecgenSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastVecgenCreate = s
	if err := f.record(ctx, "vecgen.create", key); err != nil {
		return provisioner.VecgenSubscriber{}, err
	}
	if _, ok := f.vecgen[s.IMSI]; ok {
		return provisioner.VecgenSubscriber{}, pvError(409, provisioner.CauseSubscriberExists)
	}
	v := provisioner.VecgenSubscriber{IMSI: s.IMSI, State: s.State, SQN: s.SQN, AMF: s.AMF, SQNType: s.SQNType, AllowedClientIDs: s.AllowedClientIDs}
	f.vecgen[s.IMSI] = v
	return v, f.applyErrs["vecgen.create"]
}

func (f *fakePV) UpdateVecgenSubscriber(ctx context.Context, imsi string, u provisioner.VecgenSubscriberUpdate, key string) (provisioner.VecgenSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastVecgenUpdate = u
	if err := f.record(ctx, "vecgen.update", key); err != nil {
		return provisioner.VecgenSubscriber{}, err
	}
	v, ok := f.vecgen[imsi]
	if !ok {
		return v, notFound()
	}
	v.AMF, v.SQN, v.SQNType = cmpOr(u.AMF, v.AMF), cmpOr(u.SQN, v.SQN), cmpOr(u.SQNType, v.SQNType)
	if u.AllowedClientIDs != nil {
		v.AllowedClientIDs = *u.AllowedClientIDs
	}
	f.vecgen[imsi] = v
	return v, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (f *fakePV) VecgenClients(context.Context) ([]provisioner.VecgenClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.clients), f.errs["vecgen.clients"]
}

func (f *fakePV) RadiusSubscriber(_ context.Context, imsi string) (provisioner.RadiusSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["radius.get"]; err != nil {
		return provisioner.RadiusSubscriber{}, err
	}
	s, ok := f.radius[imsi]
	if !ok {
		return s, notFound()
	}
	return s, nil
}

func (f *fakePV) RadiusBatchGet(_ context.Context, imsis []string) (provisioner.RadiusBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	if err := f.errs["radius.batch"]; err != nil {
		return provisioner.RadiusBatch{}, err
	}
	var b provisioner.RadiusBatch
	for _, imsi := range imsis {
		if s, ok := f.radius[imsi]; ok {
			b.Items = append(b.Items, s)
		} else {
			b.NotFound = append(b.NotFound, imsi)
		}
	}
	return b, nil
}

func (f *fakePV) CreateRadiusSubscriber(ctx context.Context, s provisioner.RadiusSubscriberCreate, key string) (provisioner.RadiusSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, "radius.create", key); err != nil {
		return provisioner.RadiusSubscriber{}, err
	}
	if _, ok := f.radius[s.IMSI]; ok {
		return provisioner.RadiusSubscriber{}, pvError(409, provisioner.CauseSubscriberExists)
	}
	v := provisioner.RadiusSubscriber{IMSI: s.IMSI, State: s.State, SourceID: s.SourceID, Rules: s.Rules}
	f.radius[s.IMSI] = v
	return v, f.applyErrs["radius.create"]
}

func (f *fakePV) UpdateRadiusSubscriber(ctx context.Context, imsi string, u provisioner.RadiusSubscriberUpdate, key string) (provisioner.RadiusSubscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRadiusUpdate = u
	if err := f.record(ctx, "radius.update", key); err != nil {
		return provisioner.RadiusSubscriber{}, err
	}
	v, ok := f.radius[imsi]
	if !ok {
		return v, notFound()
	}
	if v.State == "terminated" {
		return v, pvError(409, provisioner.CauseSubscriberTerminated)
	}
	v.SourceID = cmpOr(u.SourceID, v.SourceID)
	if u.Rules != nil {
		v.Rules = u.Rules
	}
	f.radius[imsi] = v
	return v, nil
}

func (f *fakePV) RadiusSources(context.Context) ([]provisioner.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sources), f.errs["radius.sources"]
}

func (f *fakePV) Sessions(_ context.Context, p provisioner.SessionParams) (provisioner.SessionList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSessions = p
	if err := f.errs["radius.sessions"]; err != nil {
		return provisioner.SessionList{}, err
	}
	var l provisioner.SessionList
	for _, s := range f.sessions {
		if (p.TenantID != 0 && s.TenantID == p.TenantID) || slices.Contains(p.IMSIs, s.IMSI) {
			l.Items = append(l.Items, s)
		}
	}
	return l, nil
}

// side は口の下流の加入者の状態を読み書きする。
func (f *fakePV) state(d provisioner.Downstream, imsi string) (string, bool) {
	if d == provisioner.Vecgen {
		s, ok := f.vecgen[imsi]
		return s.State, ok
	}
	s, ok := f.radius[imsi]
	return s.State, ok
}

func (f *fakePV) setState(d provisioner.Downstream, imsi, state string) {
	if d == provisioner.Vecgen {
		s := f.vecgen[imsi]
		s.State = state
		f.vecgen[imsi] = s
		return
	}
	s := f.radius[imsi]
	s.State = state
	f.radius[imsi] = s
}

func (f *fakePV) SetState(ctx context.Context, d provisioner.Downstream, imsi string, ch provisioner.StateChange, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, string(d)+".state."+ch.State, key); err != nil {
		return "", err
	}
	cur, ok := f.state(d, imsi)
	switch {
	case !ok:
		return "", notFound()
	case cur == "terminated" && ch.State != "terminated":
		return "", pvError(409, provisioner.CauseStateTransitionNotAllowed)
	}
	f.setState(d, imsi, ch.State)
	return ch.State, f.applyErrs[string(d)+".state"]
}

func (f *fakePV) Reinstate(ctx context.Context, d provisioner.Downstream, imsi, reason, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, string(d)+".reinstate", key); err != nil {
		return "", err
	}
	cur, ok := f.state(d, imsi)
	switch {
	case !ok:
		return "", notFound()
	case cur != "terminated":
		return "", pvError(409, provisioner.CauseStateTransitionNotAllowed)
	}
	f.setState(d, imsi, "inactive")
	return "inactive", nil
}

func (f *fakePV) DeleteSubscriber(ctx context.Context, d provisioner.Downstream, imsi, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record(ctx, string(d)+".delete", key); err != nil {
		return err
	}
	cur, ok := f.state(d, imsi)
	switch {
	case !ok:
		return notFound()
	case cur == "active":
		return pvError(409, provisioner.CauseSubscriberActive)
	}
	if d == provisioner.Vecgen {
		delete(f.vecgen, imsi)
	} else {
		delete(f.radius, imsi)
	}
	return nil
}

func (f *fakePV) AuditLogs(_ context.Context, src provisioner.AuditSource, _ provisioner.AuditLogParams) (provisioner.AuditLogList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return provisioner.AuditLogList{Items: f.audit[src]}, f.errs["audit"]
}
