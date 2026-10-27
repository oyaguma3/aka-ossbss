package provisioner

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// 各操作の型と呼び出し。provisioner だけの部分（状態、加入者の統合操作、操作の記録、監査ログ）と、
// provisioner が本PoCの Provisioning API と同じ形で中継する部分（RADIUSクライアント、認可ポリシー、セッション、
// provisioning-api の監査ログ）がある。書き込みは、idempotencyKey が空でなければ Idempotency-Key として送る
// （送り直しても provisioner は二重に処理せず、最初の応答を返す。設計概要 §10）。

// ---- 一覧の条件 ----

// ListParams は加入者・認可ポリシーの一覧の条件（IMSI の順のページング）。ゼロ値の項目は指定しない。
type ListParams struct {
	// Prefix は IMSI の前方一致条件。
	Prefix string
	// Cursor は前のページの NextCursor。
	Cursor string
	// Limit は 1 ページの件数（1〜500、省略時は 50）。
	Limit int
}

func (p ListParams) query() url.Values {
	q := url.Values{}
	setString(q, "prefix", p.Prefix)
	setString(q, "cursor", p.Cursor)
	setInt(q, "limit", int64(p.Limit))
	return q
}

// AuditLogParams は監査ログ（provisioner、provisioning-api、aka-vecgen の 3 種類で共通）の一覧の条件。
// ゼロ値の項目は指定しない。
type AuditLogParams struct {
	// Before は前のページの NextBefore（このエントリID より古いものを返す）。
	Before string
	// Limit は 1 ページの件数（1〜500、省略時は 100）。
	Limit int
}

func (p AuditLogParams) query() url.Values {
	q := url.Values{}
	setString(q, "before", p.Before)
	setInt(q, "limit", int64(p.Limit))
	return q
}

// ---- 状態 ----

// KeyStore は鍵（Ki / OPc）の置き場所。
type KeyStore string

const (
	// KeyStorePoC は本PoCの Vector API（接続方式 00。provisioning-api の加入者）。
	KeyStorePoC KeyStore = "poc"
	// KeyStoreAKA は aka-vecgen（接続方式 01。aka-vecgen の加入者）。
	KeyStoreAKA KeyStore = "aka"
)

// Status は provisioner の状態。
type Status struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	// PLMNMap は PLMN マップ（本PoCの VECTOR_GATEWAY_PLMN_MAP と同じ値のはず）。
	PLMNMap     []PLMNEntry      `json:"plmnMap"`
	Downstreams Downstreams      `json:"downstreams"`
	AVClient    AVClientStatus   `json:"avClient"`
	Valkey      ValkeyStatus     `json:"valkey"`
	Operations  *OperationCounts `json:"operations"`
}

// PLMNEntry は PLMN マップの 1 件。
type PLMNEntry struct {
	PLMN     string   `json:"plmn"`
	KeyStore KeyStore `json:"keyStore"`
}

// Downstreams は下流 2 つへの接続の状態。
type Downstreams struct {
	Prov DownstreamStatus `json:"prov"`
	Aka  DownstreamStatus `json:"aka"`
}

// DownstreamStatus は下流 1 つへの接続の状態。
type DownstreamStatus struct {
	// Configured は接続先を設定しているか（aka-vecgen を使わない設定なら aka は false）。
	Configured      bool   `json:"configured"`
	URL             string `json:"url"`
	Reachable       bool   `json:"reachable"`
	Version         string `json:"version"`
	NodeName        string `json:"nodeName"`
	SubscriberCount int64  `json:"subscriberCount"`
	// Error と Hint は接続できなかった場合のエラーと原因の見当（provisioner が日本語で返す）。
	Error string `json:"error"`
	Hint  string `json:"hint"`
}

// AVClientStatus は vector-gateway の AVクライアント（provisioner の PROVISIONER_AKA_AV_CLIENT_ID）の確認結果。
type AVClientStatus struct {
	// ID は設定の ID（aka-vecgen を使わない設定なら 0）。
	ID      int64  `json:"id"`
	Exists  bool   `json:"exists"`
	Enabled bool   `json:"enabled"`
	Name    string `json:"name"`
}

// ValkeyStatus は provisioner 専用の Valkey への接続の状態。
type ValkeyStatus struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error"`
}

// OperationCounts は未解決の操作の件数。
type OperationCounts struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
	Failed   int `json:"failed"`
}

// IsProvisioner は、この状態が provisioner の応答かを返す。provisioner は provisioning-api への接続を必ず設定しているので、
// downstreams.prov.configured が false なら provisioner の応答ではない（本PoCの provisioning-api につながっている等）。
func (s Status) IsProvisioner() bool { return s.Downstreams.Prov.Configured }

// Status は provisioner の状態を取得する。
func (c *Client) Status(ctx context.Context) (Status, error) {
	return c.call[Status](ctx, request{method: http.MethodGet, path: []string{"status"}})
}

// ---- 加入者（統合操作） ----

// Issue は 2 つのノードの状態の食い違い。
type Issue string

const (
	// IssueKeyMissing は、置き場所に加入者（鍵）がないこと。
	IssueKeyMissing Issue = "KEY_MISSING"
	// IssuePolicyMissing は、認可ポリシーがないこと（本PoCは認証を拒否する）。
	IssuePolicyMissing Issue = "POLICY_MISSING"
	// IssueKeyInOtherStore は、置き場所でない方にも同じ IMSI の加入者があること。
	IssueKeyInOtherStore Issue = "KEY_IN_OTHER_STORE"
	// IssueAVClientNotAllowed は、aka-vecgen の加入者が vector-gateway の AVクライアントを許可していないこと。
	IssueAVClientNotAllowed Issue = "AV_CLIENT_NOT_ALLOWED"
	// IssueOtherStoreUnreachable は、置き場所でない方の下流に接続できず、KEY_IN_OTHER_STORE を確かめられなかったこと。
	IssueOtherStoreUnreachable Issue = "OTHER_STORE_UNREACHABLE"
)

// Key は置き場所の加入者の属性（Ki と OPc は含まない）。SQNType 以降は aka-vecgen の加入者だけ。
type Key struct {
	AMF              string    `json:"amf"`
	SQN              string    `json:"sqn"`
	SQNType          string    `json:"sqnType,omitempty"`
	AllowPlain       *bool     `json:"allowPlain,omitempty"`
	AllowedClientIDs []int64   `json:"allowedClientIds,omitempty"`
	CreatedAt        time.Time `json:"createdAt,omitzero"`
	UpdatedAt        time.Time `json:"updatedAt,omitzero"`
}

// Subscriber は加入者（IMSI＋鍵の置き場所＋認可ポリシー）。
type Subscriber struct {
	IMSI     string   `json:"imsi"`
	KeyStore KeyStore `json:"keyStore"`
	// Key は置き場所の加入者。ない場合は nil（Issues に KEY_MISSING）。
	Key *Key `json:"key"`
	// Policy は認可ポリシー。ない場合は nil（Issues に POLICY_MISSING）。
	Policy *PolicyPut `json:"policy"`
	// Status は加入者の状態（認可ポリシーの状態。PolicyActive / PolicySuspended。provisioner 0.3.0 から）。
	// 認可ポリシーがない、または 0.2.0 以前の provisioner では空。
	Status string `json:"status"`
	// Issues は食い違い。正常なら空。
	Issues []Issue `json:"issues"`
}

// SubscriberCreate は加入者の作成の内容。認可ポリシーは必須。
type SubscriberCreate struct {
	IMSI string `json:"imsi"`
	// KeyStore は省略すれば provisioner が PLMN マップから決める。
	KeyStore KeyStore `json:"keyStore,omitempty"`
	Ki       string   `json:"ki"`
	OPc      string   `json:"opc"`
	// AMF と SQN は省略すれば 8000、000000000000。
	AMF string `json:"amf,omitempty"`
	SQN string `json:"sqn,omitempty"`
	// SQNType と AllowPlain は aka-vecgen の加入者だけ指定できる。
	SQNType    string    `json:"sqnType,omitempty"`
	AllowPlain *bool     `json:"allowPlain,omitempty"`
	Policy     PolicyPut `json:"policy"`
}

// SubscriberUpdate は加入者の変更の内容（JSON Merge Patch。空の項目は送らない）。
type SubscriberUpdate struct {
	Ki  string `json:"ki,omitempty"`
	OPc string `json:"opc,omitempty"`
	AMF string `json:"amf,omitempty"`
	SQN string `json:"sqn,omitempty"`
	// SQNType と AllowPlain は aka-vecgen の加入者だけ指定できる。
	SQNType    string `json:"sqnType,omitempty"`
	AllowPlain *bool  `json:"allowPlain,omitempty"`
	// Policy は認可ポリシー全体を置き換える（ない場合は作成する）。
	Policy *PolicyPut `json:"policy,omitempty"`
}

// SubscriberList は加入者の一覧の 1 ページ。provisioner は総数を返さない。
type SubscriberList struct {
	Items []Subscriber `json:"items"`
	// NextCursor は次のページがある場合だけ入る。
	NextCursor string `json:"nextCursor"`
}

// ListSubscribers は加入者（鍵だけ・ポリシーだけの IMSI を含む）の一覧を IMSI の昇順で取得する。
func (c *Client) ListSubscribers(ctx context.Context, p ListParams) (SubscriberList, error) {
	return c.call[SubscriberList](ctx, request{method: http.MethodGet, path: []string{"subscribers"}, query: p.query()})
}

// GetSubscriber は加入者を取得する。どこにもなければ 404（USER_NOT_FOUND）。
func (c *Client) GetSubscriber(ctx context.Context, imsi string) (Subscriber, error) {
	return c.call[Subscriber](ctx, request{method: http.MethodGet, path: []string{"subscribers", imsi}})
}

// CreateSubscriber は加入者を作成する（置き場所に鍵を作り、認可ポリシーを PUT する）。
// idempotencyKey が空でなければ Idempotency-Key として送る（送り直しても二重に処理されない）。
func (c *Client) CreateSubscriber(ctx context.Context, s SubscriberCreate, idempotencyKey string) (Subscriber, error) {
	return c.call[Subscriber](ctx, request{method: http.MethodPost, path: []string{"subscribers"}, body: s,
		needOperator: true, idempotencyKey: idempotencyKey})
}

// UpdateSubscriber は加入者を変更する。
func (c *Client) UpdateSubscriber(ctx context.Context, imsi string, u SubscriberUpdate, idempotencyKey string) (Subscriber, error) {
	return c.call[Subscriber](ctx, request{method: http.MethodPatch, path: []string{"subscribers", imsi}, body: u,
		contentType: "application/merge-patch+json", needOperator: true, idempotencyKey: idempotencyKey})
}

// DeleteSubscriber は加入者を削除する（認可ポリシーと、置き場所の鍵を消す）。
func (c *Client) DeleteSubscriber(ctx context.Context, imsi string, idempotencyKey string) error {
	_, err := c.call[struct{}](ctx, request{method: http.MethodDelete, path: []string{"subscribers", imsi},
		needOperator: true, idempotencyKey: idempotencyKey})
	return err
}

// SubscriberKeys は加入者の Ki と OPc。
type SubscriberKeys struct {
	Ki  string `json:"ki"`
	OPc string `json:"opc"`
}

// GetSubscriberKeys は加入者の Ki と OPc を、鍵の置き場所から取得する。provisioner と下流の監査ログに残る。操作者が必要。
func (c *Client) GetSubscriberKeys(ctx context.Context, imsi string) (SubscriberKeys, error) {
	return c.call[SubscriberKeys](ctx, request{
		method: http.MethodGet, path: []string{"subscribers", imsi, "keys"}, needOperator: true,
	})
}

// ---- 操作の記録 ----

// 操作の状態。
const (
	OpRunning    = "running"
	OpCompleted  = "completed"
	OpRolledBack = "rolled_back"
	OpRetrying   = "retrying"
	OpFailed     = "failed"
	OpDismissed  = "dismissed"
)

// Operation は操作の記録（2 つのノードにまたがる加入者の作成・変更・削除）。
type Operation struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	IMSI     string          `json:"imsi"`
	KeyStore KeyStore        `json:"keyStore"`
	Status   string          `json:"status"`
	Steps    []OperationStep `json:"steps"`
	// Attempts は補償・やり直しを試みた回数（最初の要求を含まない）。
	Attempts int `json:"attempts"`
	// NextAttemptAt は次に自動でやり直す時刻（retrying のときだけ）。
	NextAttemptAt time.Time `json:"nextAttemptAt,omitzero"`
	// Operator、MgmtClient、TraceID は最初の要求のもの。
	Operator   string    `json:"operator"`
	MgmtClient string    `json:"mgmtClient"`
	TraceID    string    `json:"traceId"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// OperationStep は操作の手順。
type OperationStep struct {
	Name       string     `json:"name"`
	Downstream string     `json:"downstream"`
	State      string     `json:"state"`
	Error      *StepError `json:"error"`
}

// StepError は手順の最後の失敗。
type StepError struct {
	// Status は下流の HTTP ステータス（接続できなかった場合は 0）。
	Status int       `json:"status"`
	Cause  string    `json:"cause"`
	Detail string    `json:"detail"`
	Time   time.Time `json:"time"`
}

// OperationList は未解決の操作の一覧。
type OperationList struct {
	Items []Operation `json:"items"`
	// Total は条件に一致する未解決の操作の件数（Items は limit 件まで）。
	Total int `json:"total"`
}

// ListOperations は未解決（running / retrying / failed）の操作を、作成の古い順に取得する。
// status が空でなければその状態だけ、limit が 0 なら provisioner の既定（100 件）。
func (c *Client) ListOperations(ctx context.Context, status string, limit int) (OperationList, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.call[OperationList](ctx, request{method: http.MethodGet, path: []string{"operations"}, query: q})
}

// GetOperation は操作を取得する（完了したものも保持期間のあいだ取得できる）。
func (c *Client) GetOperation(ctx context.Context, id string) (Operation, error) {
	return c.call[Operation](ctx, request{method: http.MethodGet, path: []string{"operations", id}})
}

// RetryOperation は failed の操作の続きをその場で 1 回行う。
func (c *Client) RetryOperation(ctx context.Context, id, idempotencyKey string) (Operation, error) {
	return c.call[Operation](ctx, request{method: http.MethodPost, path: []string{"operations", id, "retry"},
		needOperator: true, idempotencyKey: idempotencyKey})
}

// DismissOperation は retrying / failed の操作を、手で直した後に閉じる（下流には何もしない）。
func (c *Client) DismissOperation(ctx context.Context, id, idempotencyKey string) (Operation, error) {
	return c.call[Operation](ctx, request{method: http.MethodPost, path: []string{"operations", id, "dismiss"},
		needOperator: true, idempotencyKey: idempotencyKey})
}

// ---- RADIUSクライアント（中継） ----

// RADIUSClient は RADIUSクライアント（AP など）。共有シークレットは含まない。
type RADIUSClient struct {
	// ID はサーバー採番の ID（1 からの連番。再利用しない）。IP を変えても変わらない。
	ID     int64  `json:"id"`
	IP     string `json:"ip"`
	Name   string `json:"name"`
	Vendor string `json:"vendor"`
}

// RADIUSClientCreate は RADIUSクライアントの登録内容。
type RADIUSClientCreate struct {
	IP     string `json:"ip"`
	Secret string `json:"secret"`
	Name   string `json:"name"`
	Vendor string `json:"vendor"`
}

// RADIUSClientUpdate は RADIUSクライアントの変更内容（JSON Merge Patch）。nil の項目は変更しない。
type RADIUSClientUpdate struct {
	IP     *string `json:"ip,omitzero"`
	Secret *string `json:"secret,omitzero"`
	Name   *string `json:"name,omitzero"`
	Vendor *string `json:"vendor,omitzero"`
}

type radiusClientList struct {
	Items []RADIUSClient `json:"items"`
}

// ListRADIUSClients は RADIUSクライアントの全件を IP アドレスの順で取得する。
func (c *Client) ListRADIUSClients(ctx context.Context) ([]RADIUSClient, error) {
	l, err := c.call[radiusClientList](ctx, request{method: http.MethodGet, path: []string{"clients"}})
	return l.Items, err
}

// FindRADIUSClientByIP は IP アドレスで RADIUSクライアントを探す。見つからなければ ok が false。
func (c *Client) FindRADIUSClientByIP(ctx context.Context, ip string) (client RADIUSClient, ok bool, err error) {
	l, err := c.call[radiusClientList](ctx, request{
		method: http.MethodGet, path: []string{"clients"}, query: url.Values{"ip": {ip}},
	})
	if err != nil || len(l.Items) == 0 {
		return RADIUSClient{}, false, err
	}
	return l.Items[0], true, nil
}

// CreateRADIUSClient は RADIUSクライアントを登録する。ID は本PoCが採番する。操作者が必要。
func (c *Client) CreateRADIUSClient(ctx context.Context, rc RADIUSClientCreate, idempotencyKey string) (RADIUSClient, error) {
	return c.call[RADIUSClient](ctx, request{
		method: http.MethodPost, path: []string{"clients"}, body: rc, needOperator: true, idempotencyKey: idempotencyKey,
	})
}

// GetRADIUSClient は RADIUSクライアントを取得する。
func (c *Client) GetRADIUSClient(ctx context.Context, id int64) (RADIUSClient, error) {
	return c.call[RADIUSClient](ctx, request{method: http.MethodGet, path: []string{"clients", formatID(id)}})
}

// UpdateRADIUSClient は RADIUSクライアントを変更する。操作者が必要。
func (c *Client) UpdateRADIUSClient(ctx context.Context, id int64, u RADIUSClientUpdate, idempotencyKey string) (RADIUSClient, error) {
	return c.call[RADIUSClient](ctx, request{
		method: http.MethodPatch, path: []string{"clients", formatID(id)}, body: u,
		contentType: "application/merge-patch+json", needOperator: true, idempotencyKey: idempotencyKey,
	})
}

// DeleteRADIUSClient は RADIUSクライアントを削除する。操作者が必要。
func (c *Client) DeleteRADIUSClient(ctx context.Context, id int64, idempotencyKey string) error {
	_, err := c.call[struct{}](ctx, request{
		method: http.MethodDelete, path: []string{"clients", formatID(id)}, needOperator: true, idempotencyKey: idempotencyKey,
	})
	return err
}

// GetRADIUSClientSecret は RADIUSクライアントの共有シークレットを取得する。provisioner と provisioning-api の監査ログに残る。
// 操作者が必要。
func (c *Client) GetRADIUSClientSecret(ctx context.Context, id int64) (string, error) {
	s, err := c.call[struct {
		Secret string `json:"secret"`
	}](ctx, request{
		method: http.MethodGet, path: []string{"clients", formatID(id), "secret"}, needOperator: true,
	})
	return s.Secret, err
}

// ---- 認可ポリシー（中継） ----

// 認可ポリシーの既定の動作（どのルールにも一致しなかったとき）。
const (
	PolicyAllow = "allow"
	PolicyDeny  = "deny"
)

// PolicyRule は認可ポリシーのルール。先頭から順に評価し、最初に一致したものを採用する。
type PolicyRule struct {
	// NASID は NAS-Identifier。"*" は任意の NAS に一致する。
	NASID string `json:"nasId"`
	// AllowedSSIDs は許可する SSID。"*" は任意の SSID に一致する。
	AllowedSSIDs []string `json:"allowedSsids"`
	// VLANID は割り当てる VLAN ID（0〜4094 の数字）。空なら未設定。
	VLANID string `json:"vlanId,omitempty"`
	// SessionTimeout は Session-Timeout（秒）。0 なら未設定。
	SessionTimeout int `json:"sessionTimeout,omitzero"`
}

// Policy は認可ポリシー。
type Policy struct {
	IMSI    string       `json:"imsi"`
	Default string       `json:"default"`
	Rules   []PolicyRule `json:"rules"`
	// Status は加入者の状態（本PoCの Provisioning API 0.4.0 から）。PolicyActive か PolicySuspended。
	// 0.3.0 以前の provisioning-api では空。Valkey を直接書き換えた不正な値は、そのまま入る（Auth Server は認証を拒否する）。
	Status string `json:"status,omitempty"`
}

// 加入者の状態（Policy.Status）。停止中の加入者の認証は、鍵の置き場所によらず Auth Server が拒否する。
const (
	PolicyActive    = "active"
	PolicySuspended = "suspended"
)

// PolicyPut は認可ポリシーの内容（全体を置き換える）。
type PolicyPut struct {
	Default string       `json:"default"`
	Rules   []PolicyRule `json:"rules"`
}

// PolicyList は認可ポリシーの一覧の 1 ページ。
type PolicyList struct {
	Items []Policy `json:"items"`
	// Total は Prefix に一致する認可ポリシーの総数。
	Total int64 `json:"total"`
	// NextCursor は次のページがある場合だけ入る。
	NextCursor string `json:"nextCursor"`
}

// ListPolicies は認可ポリシーの一覧を IMSI の昇順で取得する。
func (c *Client) ListPolicies(ctx context.Context, p ListParams) (PolicyList, error) {
	return c.call[PolicyList](ctx, request{method: http.MethodGet, path: []string{"policies"}, query: p.query()})
}

// GetPolicy は認可ポリシーを取得する。
func (c *Client) GetPolicy(ctx context.Context, imsi string) (Policy, error) {
	return c.call[Policy](ctx, request{method: http.MethodGet, path: []string{"policies", imsi}})
}

// PutPolicy は認可ポリシーを作成する、または全体を置き換える（状態は変わらない）。作成した場合は created が true。
// 加入者の有無は問わない。操作者が必要。
func (c *Client) PutPolicy(ctx context.Context, imsi string, p PolicyPut, idempotencyKey string) (policy Policy, created bool, err error) {
	req := request{method: http.MethodPut, path: []string{"policies", imsi}, body: p, needOperator: true, idempotencyKey: idempotencyKey}
	resp, err := c.send(ctx, req)
	if err != nil {
		return Policy{}, false, err
	}
	defer drainClose(resp.Body)
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), &policy); err != nil {
		return Policy{}, false, fmt.Errorf("%s PUT %s: decode response: %w", name, resp.Request.URL.Path, err)
	}
	return policy, resp.StatusCode == http.StatusCreated, nil
}

// SetPolicyStatus は加入者を停止する（PolicySuspended）、または再開する（PolicyActive）。変更後の認可ポリシーを返す。
// 認可ポリシーの状態だけを変える（provisioner 0.3.0 の PUT /policies/{imsi}/status）。認可ポリシーがなければ
// POLICY_NOT_FOUND。効くのは次の認証からで、接続中のセッションは切れない。操作者が必要。
// 本BFF は停止・再開にも Idempotency-Key を付ける（設計概要 §10。既存の BFF とは違う。§17.3）。
func (c *Client) SetPolicyStatus(ctx context.Context, imsi, status, idempotencyKey string) (Policy, error) {
	return c.call[Policy](ctx, request{
		method: http.MethodPut, path: []string{"policies", imsi, "status"},
		body: struct {
			Status string `json:"status"`
		}{status}, needOperator: true, idempotencyKey: idempotencyKey,
	})
}

// DeletePolicy は認可ポリシーを削除する。操作者が必要。
func (c *Client) DeletePolicy(ctx context.Context, imsi, idempotencyKey string) error {
	_, err := c.call[struct{}](ctx, request{
		method: http.MethodDelete, path: []string{"policies", imsi}, needOperator: true, idempotencyKey: idempotencyKey,
	})
	return err
}

// ---- セッション（中継） ----

// SessionParams はセッションの一覧の条件。ゼロ値の項目は指定しない。
type SessionParams struct {
	// IMSI を指定すると、その加入者のセッションだけを返す。
	IMSI string
	// Limit は返す件数の上限（1〜1000、省略時は 100）。
	Limit int
}

// Session はアクティブセッション。
type Session struct {
	// ID はセッションの UUID（RADIUS の Class 属性の値）。
	ID   string `json:"id"`
	IMSI string `json:"imsi"`
	// NasIP は NAS の IP アドレス。
	NasIP         string `json:"nasIp"`
	NasIdentifier string `json:"nasIdentifier"`
	// StartTime は接続開始日時。値を持たないセッションではゼロ値。
	StartTime time.Time `json:"startTime,omitzero"`
	// ClientIP は端末の IP アドレス（Accounting-Request を受けるまでは空文字）。
	ClientIP string `json:"clientIp"`
	// AcctSessionID は Acct-Session-Id（Accounting-Start を受けるまでは空文字）。
	AcctSessionID string `json:"acctSessionId"`
	InputOctets   int64  `json:"inputOctets"`
	OutputOctets  int64  `json:"outputOctets"`
}

// SessionList はセッションの一覧（接続開始の新しい順）。
type SessionList struct {
	Items []Session `json:"items"`
	// Total は条件に一致するセッションの総数（Items は Limit 件まで）。
	Total int64 `json:"total"`
}

// ListSessions はアクティブセッションを接続開始の新しい順に取得する。
func (c *Client) ListSessions(ctx context.Context, p SessionParams) (SessionList, error) {
	q := url.Values{}
	setString(q, "imsi", p.IMSI)
	setInt(q, "limit", int64(p.Limit))
	return c.call[SessionList](ctx, request{method: http.MethodGet, path: []string{"sessions"}, query: q})
}

// ---- 監査ログ ----

// AuditLogEntry は provisioner の監査ログの 1 件。
type AuditLogEntry struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// Operator は X-Operator-Id の値（自動のやり直しでは空文字）。
	Operator string `json:"operator"`
	// MgmtClient は管理クライアントの識別名（provisioner の PROVISIONER_ADMIN_CLIENTS の名前。自動のやり直しでは空文字）。
	MgmtClient string `json:"mgmtClient"`
	Action     string `json:"action"`
	// Target は対象（加入者・認可ポリシーは IMSI、RADIUSクライアントは ID、操作の記録は操作の ID）。
	Target      string `json:"target"`
	TraceID     string `json:"traceId"`
	OperationID string `json:"operationId"`
	// Result は結果（completed / rolled_back / retrying / failed / dismissed）。
	Result  string         `json:"result"`
	Details map[string]any `json:"details"`
}

// AuditLogList は provisioner の監査ログの 1 ページ。
type AuditLogList struct {
	Items      []AuditLogEntry `json:"items"`
	NextBefore string          `json:"nextBefore"`
}

// ListAuditLogs は provisioner 自身の監査ログを新しい順に取得する。
func (c *Client) ListAuditLogs(ctx context.Context, p AuditLogParams) (AuditLogList, error) {
	return c.call[AuditLogList](ctx, request{method: http.MethodGet, path: []string{"audit-logs"}, query: p.query()})
}

// ProvAuditLogEntry は provisioning-api の監査ログの 1 件（変更操作と秘密の値の取得）。
type ProvAuditLogEntry struct {
	// ID はエントリID（Valkey の Stream の ID）。
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// Operator は X-Operator-Id の値（省略された操作では空文字）。
	Operator string `json:"operator"`
	// MgmtClient は管理クライアントの識別名（本PoC側の PROVISIONING_API_ADMIN_CLIENTS の名前。provisioner 経由なら provisioner の名前）。
	MgmtClient string `json:"mgmtClient"`
	// Action は操作（例: subscriber.create、subscriber.keys.read、client.secret.read）。
	Action string `json:"action"`
	// Target は対象（加入者・認可ポリシーは IMSI、RADIUSクライアントは ID）。
	Target    string `json:"target"`
	TargetKey string `json:"targetKey"`
	// TraceID はトレースID（本BFF・provisioner の監査ログのトレースID と同じ）。
	TraceID string `json:"traceId"`
	// Details は変更内容（秘密の値は含まない）。
	Details string `json:"details"`
}

// ProvAuditLogList は provisioning-api の監査ログの 1 ページ（新しい順）。
type ProvAuditLogList struct {
	Items []ProvAuditLogEntry `json:"items"`
	// NextBefore はさらに古いエントリがある場合だけ入る。
	NextBefore string `json:"nextBefore"`
}

// ListProvAuditLogs は provisioning-api の監査ログ（provisioner の中継。形は provisioning-api と同じ）を新しい順に取得する。
func (c *Client) ListProvAuditLogs(ctx context.Context, p AuditLogParams) (ProvAuditLogList, error) {
	return c.call[ProvAuditLogList](ctx, request{method: http.MethodGet, path: []string{"prov", "audit-logs"}, query: p.query()})
}

// AkaAuditLogEntry は aka-vecgen の監査ログの 1 件（aka-vecgen の管理API の形）。
type AkaAuditLogEntry struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Operator   string    `json:"operator"`
	MgmtClient string    `json:"mgmtClient"`
	Action     string    `json:"action"`
	Target     string    `json:"target"`
	TraceID    string    `json:"traceId"`
	// Detail は変更内容（変更なら項目ごとの {"from": …, "to": …}）。
	Detail map[string]any `json:"detail"`
}

// AkaAuditLogList は aka-vecgen の監査ログの 1 ページ。
type AkaAuditLogList struct {
	Items      []AkaAuditLogEntry `json:"items"`
	NextBefore string             `json:"nextBefore"`
}

// ListAkaAuditLogs は aka-vecgen の監査ログ（provisioner の中継）を取得する。
// provisioner が aka-vecgen を扱わない設定なら 404（DOWNSTREAM_NOT_CONFIGURED）。
func (c *Client) ListAkaAuditLogs(ctx context.Context, p AuditLogParams) (AkaAuditLogList, error) {
	return c.call[AkaAuditLogList](ctx, request{method: http.MethodGet, path: []string{"aka", "audit-logs"}, query: p.query()})
}

// ---- 補助 ----

func formatID(id int64) string { return strconv.FormatInt(id, 10) }

func setString(q url.Values, name, v string) {
	if v != "" {
		q.Set(name, v)
	}
}

func setInt(q url.Values, name string, v int64) {
	if v != 0 {
		q.Set(name, strconv.FormatInt(v, 10))
	}
}
