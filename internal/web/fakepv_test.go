package web

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

// fakePV はテスト用の provisioner。加入者・RADIUSクライアント・セッションをメモリに持ち、
// 変更操作と秘密の値の取得ごとに「操作名 操作者 Idempotency-Key」を記録する。
type fakePV struct {
	mu     sync.Mutex
	status provisioner.Status
	// err を設定すると Status がそのエラーを返す。calls は Status の呼び出し回数。
	err   error
	calls int
	// delay は Status が応答するまでの時間（provisioner につながらずにタイムアウトする場合）。
	delay time.Duration

	subs     map[string]provisioner.Subscriber
	keys     map[string]provisioner.SubscriberKeys
	clients  map[int64]provisioner.RADIUSClient
	secrets  map[int64]string
	nextID   int64
	sessions []provisioner.Session
	// errs は操作名ごとに返すエラー（GetSubscriber、CreateSubscriber など）。
	errs map[string]error
	// ops は変更操作と秘密の値の取得の記録（「操作名 操作者 キー」）。gets は GetSubscriber の呼び出し回数。
	ops  []string
	gets int
	// lastList と lastSessions は最後に受け取った一覧の条件。lastUpdate・lastClientUpdate・lastPut は最後の変更の内容。
	lastList         provisioner.ListParams
	listCalls        int
	lastSessions     provisioner.SessionParams
	lastCreate       provisioner.SubscriberCreate
	lastUpdate       provisioner.SubscriberUpdate
	lastClientUpdate provisioner.RADIUSClientUpdate
	lastPut          provisioner.PolicyPut
}

func newFakePV() *fakePV {
	return &fakePV{status: provisioner.Status{Version: "0.3.0",
		PLMNMap: []provisioner.PLMNEntry{{PLMN: "00102", KeyStore: provisioner.KeyStoreAKA}},
		Downstreams: provisioner.Downstreams{
			Prov: provisioner.DownstreamStatus{Configured: true, Reachable: true, Version: "0.4.0", NodeName: "poc-01", SubscriberCount: 3},
			Aka:  provisioner.DownstreamStatus{Configured: true, Reachable: false, Hint: "aka-only-server に接続できません。"},
		},
		AVClient:   provisioner.AVClientStatus{ID: 1},
		Operations: &provisioner.OperationCounts{Failed: 1}},
		subs: map[string]provisioner.Subscriber{}, keys: map[string]provisioner.SubscriberKeys{},
		clients: map[int64]provisioner.RADIUSClient{}, secrets: map[int64]string{}, nextID: 1,
		errs: map[string]error{}}
}

func pvNotFound(cause string) error {
	return &provisioner.Error{Status: 404, Problem: provisioner.Problem{Status: 404, Cause: cause}}
}

func pvConflict(cause string) error {
	return &provisioner.Error{Status: 409, Problem: provisioner.Problem{Status: 409, Cause: cause}}
}

func (f *fakePV) record(ctx context.Context, op, key string) {
	f.ops = append(f.ops, strings.TrimSpace(op+" "+provisioner.OperatorFrom(ctx)+" "+key))
}

// lastOp は最後の記録を返す。
func (f *fakePV) lastOp() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ops) == 0 {
		return ""
	}
	return f.ops[len(f.ops)-1]
}

// addSubscriber は加入者（鍵を本PoC、または 00102 なら aka-only-server に置き、認可ポリシーは利用中）を加える。
func (f *fakePV) addSubscriber(imsi string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ks := keyStoreOf(imsi)
	k := &provisioner.Key{AMF: "8000", SQN: "000000000020", CreatedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	if ks == provisioner.KeyStoreAKA {
		k.SQNType, k.AllowPlain, k.AllowedClientIDs = "inc32", new(false), []int64{1}
	}
	f.subs[imsi] = provisioner.Subscriber{IMSI: imsi, KeyStore: ks, Key: k, Status: provisioner.PolicyActive, Issues: []provisioner.Issue{},
		Policy: &provisioner.PolicyPut{Default: "deny", Rules: []provisioner.PolicyRule{{NASID: "ap-01", AllowedSSIDs: []string{"corp"}, VLANID: "10"}}}}
	f.keys[imsi] = provisioner.SubscriberKeys{Ki: strings.Repeat("1", 32), OPc: strings.Repeat("2", 32)}
}

// addClient は RADIUSクライアントを加え、ID を返す。
func (f *fakePV) addClient(ip, name string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	f.nextID++
	f.clients[id] = provisioner.RADIUSClient{ID: id, IP: ip, Name: name, Vendor: "acme"}
	f.secrets[id] = "secret-" + name
	return id
}

// keyStoreOf は PLMN マップから置き場所を決める（00102 は aka、それ以外は poc）。
func keyStoreOf(imsi string) provisioner.KeyStore {
	if strings.HasPrefix(imsi, "00102") {
		return provisioner.KeyStoreAKA
	}
	return provisioner.KeyStorePoC
}

func (f *fakePV) Status(context.Context) (provisioner.Status, error) {
	f.mu.Lock()
	f.calls++
	st, err, delay := f.status, f.err, f.delay
	f.mu.Unlock()
	time.Sleep(delay)
	return st, err
}

func (f *fakePV) ListSubscribers(_ context.Context, p provisioner.ListParams) (provisioner.SubscriberList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastList = p
	f.listCalls++
	if err := f.errs["ListSubscribers"]; err != nil {
		return provisioner.SubscriberList{}, err
	}
	var l provisioner.SubscriberList
	for _, imsi := range slices.Sorted(maps.Keys(f.subs)) {
		if strings.HasPrefix(imsi, p.Prefix) && imsi > p.Cursor {
			l.Items = append(l.Items, f.subs[imsi])
		}
	}
	if limit := cmp.Or(p.Limit, 50); len(l.Items) > limit {
		l.Items = l.Items[:limit]
		l.NextCursor = l.Items[limit-1].IMSI
	}
	return l, nil
}

func (f *fakePV) GetSubscriber(_ context.Context, imsi string) (provisioner.Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if err := f.errs["GetSubscriber"]; err != nil {
		return provisioner.Subscriber{}, err
	}
	s, ok := f.subs[imsi]
	if !ok {
		return provisioner.Subscriber{}, pvNotFound(provisioner.CauseUserNotFound)
	}
	return s, nil
}

func (f *fakePV) CreateSubscriber(ctx context.Context, in provisioner.SubscriberCreate, key string) (provisioner.Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "CreateSubscriber", key)
	f.lastCreate = in
	if err := f.errs["CreateSubscriber"]; err != nil {
		return provisioner.Subscriber{}, err
	}
	if _, ok := f.subs[in.IMSI]; ok {
		return provisioner.Subscriber{}, pvConflict(provisioner.CauseSubscriberExists)
	}
	ks := keyStoreOf(in.IMSI)
	s := provisioner.Subscriber{IMSI: in.IMSI, KeyStore: ks, Key: &provisioner.Key{AMF: in.AMF, SQN: in.SQN},
		Policy: &in.Policy, Status: provisioner.PolicyActive, Issues: []provisioner.Issue{}}
	f.subs[in.IMSI] = s
	f.keys[in.IMSI] = provisioner.SubscriberKeys{Ki: in.Ki, OPc: in.OPc}
	return s, nil
}

func (f *fakePV) UpdateSubscriber(ctx context.Context, imsi string, u provisioner.SubscriberUpdate, key string) (provisioner.Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "UpdateSubscriber", key)
	f.lastUpdate = u
	if err := f.errs["UpdateSubscriber"]; err != nil {
		return provisioner.Subscriber{}, err
	}
	s, ok := f.subs[imsi]
	if !ok || s.Key == nil {
		return provisioner.Subscriber{}, pvNotFound(provisioner.CauseUserNotFound)
	}
	k := *s.Key
	k.AMF, k.SQN, k.SQNType = cmp.Or(u.AMF, k.AMF), cmp.Or(u.SQN, k.SQN), cmp.Or(u.SQNType, k.SQNType)
	if u.AllowPlain != nil {
		k.AllowPlain = new(*u.AllowPlain)
	}
	s.Key = &k
	f.subs[imsi] = s
	return s, nil
}

func (f *fakePV) DeleteSubscriber(ctx context.Context, imsi, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "DeleteSubscriber", key)
	if err := f.errs["DeleteSubscriber"]; err != nil {
		return err
	}
	if _, ok := f.subs[imsi]; !ok {
		return pvNotFound(provisioner.CauseUserNotFound)
	}
	delete(f.subs, imsi)
	return nil
}

func (f *fakePV) GetSubscriberKeys(ctx context.Context, imsi string) (provisioner.SubscriberKeys, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "GetSubscriberKeys", "")
	if err := f.errs["GetSubscriberKeys"]; err != nil {
		return provisioner.SubscriberKeys{}, err
	}
	k, ok := f.keys[imsi]
	if !ok {
		return provisioner.SubscriberKeys{}, pvNotFound(provisioner.CauseUserNotFound)
	}
	return k, nil
}

func (f *fakePV) PutPolicy(ctx context.Context, imsi string, p provisioner.PolicyPut, key string) (provisioner.Policy, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "PutPolicy", key)
	f.lastPut = p
	if err := f.errs["PutPolicy"]; err != nil {
		return provisioner.Policy{}, false, err
	}
	s, ok := f.subs[imsi]
	if !ok {
		return provisioner.Policy{}, false, pvNotFound(provisioner.CauseUserNotFound)
	}
	created := s.Policy == nil
	if created {
		s.Status = provisioner.PolicyActive
	}
	s.Policy = &p
	f.subs[imsi] = s
	return provisioner.Policy{IMSI: imsi, Default: p.Default, Rules: p.Rules, Status: s.Status}, created, nil
}

func (f *fakePV) SetPolicyStatus(ctx context.Context, imsi, status, key string) (provisioner.Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "SetPolicyStatus "+status, key)
	if err := f.errs["SetPolicyStatus"]; err != nil {
		return provisioner.Policy{}, err
	}
	s, ok := f.subs[imsi]
	if !ok || s.Policy == nil {
		return provisioner.Policy{}, pvNotFound(provisioner.CausePolicyNotFound)
	}
	s.Status = status
	f.subs[imsi] = s
	return provisioner.Policy{IMSI: imsi, Default: s.Policy.Default, Rules: s.Policy.Rules, Status: status}, nil
}

func (f *fakePV) ListRADIUSClients(context.Context) ([]provisioner.RADIUSClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ListRADIUSClients"]; err != nil {
		return nil, err
	}
	out := slices.Collect(maps.Values(f.clients))
	slices.SortFunc(out, func(a, b provisioner.RADIUSClient) int { return cmp.Compare(a.IP, b.IP) })
	return out, nil
}

func (f *fakePV) CreateRADIUSClient(ctx context.Context, rc provisioner.RADIUSClientCreate, key string) (provisioner.RADIUSClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "CreateRADIUSClient", key)
	if err := f.errs["CreateRADIUSClient"]; err != nil {
		return provisioner.RADIUSClient{}, err
	}
	for _, c := range f.clients {
		if c.IP == rc.IP {
			return provisioner.RADIUSClient{}, pvConflict(provisioner.CauseClientExists)
		}
	}
	c := provisioner.RADIUSClient{ID: f.nextID, IP: rc.IP, Name: rc.Name, Vendor: rc.Vendor}
	f.nextID++
	f.clients[c.ID] = c
	f.secrets[c.ID] = rc.Secret
	return c, nil
}

func (f *fakePV) GetRADIUSClient(_ context.Context, id int64) (provisioner.RADIUSClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.clients[id]
	if !ok {
		return provisioner.RADIUSClient{}, pvNotFound(provisioner.CauseClientNotFound)
	}
	return c, nil
}

func (f *fakePV) UpdateRADIUSClient(ctx context.Context, id int64, u provisioner.RADIUSClientUpdate, key string) (provisioner.RADIUSClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "UpdateRADIUSClient", key)
	f.lastClientUpdate = u
	if err := f.errs["UpdateRADIUSClient"]; err != nil {
		return provisioner.RADIUSClient{}, err
	}
	c, ok := f.clients[id]
	if !ok {
		return provisioner.RADIUSClient{}, pvNotFound(provisioner.CauseClientNotFound)
	}
	if u.IP != nil {
		c.IP = *u.IP
	}
	if u.Name != nil {
		c.Name = *u.Name
	}
	if u.Vendor != nil {
		c.Vendor = *u.Vendor
	}
	if u.Secret != nil {
		f.secrets[id] = *u.Secret
	}
	f.clients[id] = c
	return c, nil
}

func (f *fakePV) DeleteRADIUSClient(ctx context.Context, id int64, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "DeleteRADIUSClient", key)
	if _, ok := f.clients[id]; !ok {
		return pvNotFound(provisioner.CauseClientNotFound)
	}
	delete(f.clients, id)
	return nil
}

func (f *fakePV) GetRADIUSClientSecret(ctx context.Context, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(ctx, "GetRADIUSClientSecret", "")
	s, ok := f.secrets[id]
	if !ok {
		return "", pvNotFound(provisioner.CauseClientNotFound)
	}
	return s, nil
}

func (f *fakePV) ListSessions(_ context.Context, p provisioner.SessionParams) (provisioner.SessionList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSessions = p
	if err := f.errs["ListSessions"]; err != nil {
		return provisioner.SessionList{}, err
	}
	var l provisioner.SessionList
	for _, s := range f.sessions {
		if p.IMSI == "" || s.IMSI == p.IMSI {
			l.Total++
			if len(l.Items) < cmp.Or(p.Limit, 100) {
				l.Items = append(l.Items, s)
			}
		}
	}
	return l, nil
}
