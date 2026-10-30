package provisioner

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 各口の型と呼び出し。provisioner 自身の口（/status、/audit-logs）と、provisioner が中継する aka-vecgen・aka-radius の
// 管理API 2.0.0 の口（/vecgen/...、/radius/...）がある。書き込みは Idempotency-Key が必須（送り直しても provisioner は
// 二重に処理せず、最初の応答を返す）。

// Downstream は下流（provisioner の中継のパスの最初の要素）。
type Downstream string

const (
	Vecgen Downstream = "vecgen"
	Radius Downstream = "radius"
)

// ---- provisioner の状態 ----

// Certificate は証明書の要約。
type Certificate struct {
	Subject     string    `json:"subject"`
	NotAfter    time.Time `json:"notAfter,omitzero"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

// Status は provisioner の状態。
type Status struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	Valkey    struct {
		Reachable bool   `json:"reachable"`
		Error     string `json:"error,omitempty"`
	} `json:"valkey"`
	AdminClients         []string           `json:"adminClients"`
	AdminClientsLoadedAt time.Time          `json:"adminClientsLoadedAt"`
	DownstreamsLoadedAt  time.Time          `json:"downstreamsLoadedAt"`
	ClientCertificate    Certificate        `json:"clientCertificate"`
	ServerCertificate    Certificate        `json:"serverCertificate"`
	Downstreams          []DownstreamStatus `json:"downstreams"`
}

// DownstreamStatus は provisioner が 30 秒ごとに集めた下流 1 つの状態。
type DownstreamStatus struct {
	Name              Downstream  `json:"name"`
	URL               string      `json:"url"`
	ServerCertificate Certificate `json:"serverCertificate"`
	Reachable         bool        `json:"reachable"`
	CheckedAt         time.Time   `json:"checkedAt,omitzero"`
	// Error と Hint は接続できなかったときの誤りと原因の見当（provisioner が日本語で返す）。
	Error string `json:"error,omitempty"`
	Hint  string `json:"hint,omitempty"`
	// Status は下流の /status の本文（provisioner は解釈しない）。Node で主な項目を読む。
	Status jsontext.Value `json:"status,omitempty"`
}

// NodeStatus は下流の /status の、画面に出す項目（aka-vecgen と aka-radius で共通のもの）。
type NodeStatus struct {
	Version         string `json:"version"`
	Build           string `json:"build"`
	NodeName        string `json:"nodeName"`
	SubscriberCount int64  `json:"subscriberCount"`
	ClientCount     int64  `json:"clientCount"`
	// SessionCount は aka-radius だけ（数えていなければ nil）。
	SessionCount *int64 `json:"sessionCount"`
	// AVAPIContractVersion は aka-vecgen だけ。
	AVAPIContractVersion string `json:"avApiContractVersion"`
}

// Node は下流の /status の主な項目を読む（読めなければ false）。
func (d DownstreamStatus) Node() (NodeStatus, bool) {
	var n NodeStatus
	if len(d.Status) == 0 || json.Unmarshal(d.Status, &n) != nil {
		return NodeStatus{}, false
	}
	return n, true
}

// IsProvisioner は、この状態が provisioner 2.0.0 の応答かを返す（下流の登録簿を必ず持つ）。
func (s Status) IsProvisioner() bool { return s.Version != "" && len(s.Downstreams) > 0 }

// Status は provisioner の状態を取得する（下流の状態は provisioner が 30 秒ごとに集めたもの）。
func (c *Client) Status(ctx context.Context) (Status, error) {
	return c.call[Status](ctx, request{method: http.MethodGet, path: []string{"status"}})
}

// ---- 加入者（両側に共通の口） ----

// StateChange は状態の変更（PUT /subscribers/{imsi}/state）。
type StateChange struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// reinstate は Terminated からの戻し（POST /subscribers/{imsi}/reinstate）。
type reinstate struct {
	Reason string `json:"reason"`
}

// subscriberState は状態の変更と戻しの応答の、状態だけ。
type subscriberState struct {
	State string `json:"state"`
}

// SetState はその側の加入者の状態を変え、変えた後の状態を返す。
func (c *Client) SetState(ctx context.Context, d Downstream, imsi string, ch StateChange, idempotencyKey string) (string, error) {
	s, err := c.call[subscriberState](ctx, request{method: http.MethodPut, path: []string{string(d), "subscribers", imsi, "state"},
		body: ch, write: true, idempotencyKey: idempotencyKey})
	return s.State, err
}

// Reinstate はその側の Terminated の加入者を Inactive に戻し、戻した後の状態を返す（理由は必須）。
func (c *Client) Reinstate(ctx context.Context, d Downstream, imsi, reason, idempotencyKey string) (string, error) {
	s, err := c.call[subscriberState](ctx, request{method: http.MethodPost, path: []string{string(d), "subscribers", imsi, "reinstate"},
		body: reinstate{Reason: reason}, write: true, idempotencyKey: idempotencyKey})
	return s.State, err
}

// DeleteSubscriber はその側の加入者を物理削除する（Active は下流が断る）。
func (c *Client) DeleteSubscriber(ctx context.Context, d Downstream, imsi, idempotencyKey string) error {
	_, err := c.call[struct{}](ctx, request{method: http.MethodDelete, path: []string{string(d), "subscribers", imsi},
		write: true, idempotencyKey: idempotencyKey})
	return err
}

// batchGetRequest は一括取得の要求（IMSI は 1〜500 件）。
type batchGetRequest struct {
	IMSIs []string `json:"imsis"`
}

// MaxBatchGet は一括取得の 1 回の IMSI の数の上限。
const MaxBatchGet = 500

// ---- aka-vecgen ----

// VecgenSubscriber は aka-vecgen の加入者（鍵は含まない）。
type VecgenSubscriber struct {
	IMSI             string    `json:"imsi"`
	State            string    `json:"state"`
	StateChangedAt   time.Time `json:"stateChangedAt"`
	SQN              string    `json:"sqn"`
	AMF              string    `json:"amf"`
	SQNType          string    `json:"sqnType"`
	AllowedClientIDs []int64   `json:"allowedClientIds"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// VecgenSubscriberCreate は aka-vecgen の加入者の登録の内容。Ki と OPc は aka-vecgen に送るだけで、どこにも残さない。
type VecgenSubscriberCreate struct {
	IMSI  string `json:"imsi"`
	Ki    string `json:"ki"`
	OPc   string `json:"opc"`
	State string `json:"state"`
	// SQN・AMF・SQNType は空なら下流の既定（000000000000、8000、inc32）。
	SQN              string  `json:"sqn,omitempty"`
	AMF              string  `json:"amf,omitempty"`
	SQNType          string  `json:"sqnType,omitempty"`
	AllowedClientIDs []int64 `json:"allowedClientIds"`
}

// VecgenSubscriberUpdate は aka-vecgen の加入者の変更（JSON Merge Patch。空の項目は送らない）。
type VecgenSubscriberUpdate struct {
	SQN     string `json:"sqn,omitempty"`
	AMF     string `json:"amf,omitempty"`
	SQNType string `json:"sqnType,omitempty"`
	// AllowedClientIDs は nil なら送らない（空の集合にするときは空のスライスへのポインター）。
	AllowedClientIDs *[]int64 `json:"allowedClientIds,omitzero"`
}

// VecgenBatch は aka-vecgen の一括取得の結果（要求の順）。
type VecgenBatch struct {
	Items    []VecgenSubscriber `json:"items"`
	NotFound []string           `json:"notFound"`
}

// VecgenClient は aka-vecgen の AVクライアント（許可クライアントの選択肢）。
type VecgenClient struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	FingerprintSHA256 string    `json:"fingerprintSha256"`
	Subject           string    `json:"subject"`
	NotBefore         time.Time `json:"notBefore"`
	NotAfter          time.Time `json:"notAfter"`
	Enabled           bool      `json:"enabled"`
	NetworkName       string    `json:"networkName"`
}

// VecgenSubscriber は aka-vecgen の加入者を取得する（なければ 404 USER_NOT_FOUND）。
func (c *Client) VecgenSubscriber(ctx context.Context, imsi string) (VecgenSubscriber, error) {
	return c.call[VecgenSubscriber](ctx, request{method: http.MethodGet, path: []string{"vecgen", "subscribers", imsi}})
}

// VecgenBatchGet は aka-vecgen の加入者を一括で取得する（IMSI は 1〜500 件）。
func (c *Client) VecgenBatchGet(ctx context.Context, imsis []string) (VecgenBatch, error) {
	return c.call[VecgenBatch](ctx, request{method: http.MethodPost, path: []string{"vecgen", "subscribers", "batch-get"},
		body: batchGetRequest{IMSIs: imsis}})
}

// VecgenList は aka-vecgen の加入者の一覧（IMSI の辞書順）。
type VecgenList struct {
	Items []VecgenSubscriber `json:"items"`
	Total int64              `json:"total"`
	// NextCursor は次のページがあるときの位置。
	NextCursor string `json:"nextCursor,omitempty"`
}

// ListVecgenSubscribers は aka-vecgen の加入者を IMSI の辞書順に返す（limit は 1〜500。突き合わせに使う）。
func (c *Client) ListVecgenSubscribers(ctx context.Context, cursor string, limit int) (VecgenList, error) {
	q := url.Values{}
	setString(q, "cursor", cursor)
	setInt(q, "limit", int64(limit))
	return c.call[VecgenList](ctx, request{method: http.MethodGet, path: []string{"vecgen", "subscribers"}, query: q})
}

// CreateVecgenSubscriber は aka-vecgen に加入者を登録する。
func (c *Client) CreateVecgenSubscriber(ctx context.Context, s VecgenSubscriberCreate, idempotencyKey string) (VecgenSubscriber, error) {
	return c.call[VecgenSubscriber](ctx, request{method: http.MethodPost, path: []string{"vecgen", "subscribers"},
		body: s, write: true, idempotencyKey: idempotencyKey})
}

// UpdateVecgenSubscriber は aka-vecgen の加入者を変更する。
func (c *Client) UpdateVecgenSubscriber(ctx context.Context, imsi string, u VecgenSubscriberUpdate, idempotencyKey string) (VecgenSubscriber, error) {
	return c.call[VecgenSubscriber](ctx, request{method: http.MethodPatch, path: []string{"vecgen", "subscribers", imsi},
		body: u, contentType: mergePatch, write: true, idempotencyKey: idempotencyKey})
}

// clientList は AVクライアントの一覧の応答。
type clientList[T any] struct {
	Items []T `json:"items"`
}

// VecgenClients は aka-vecgen の AVクライアントを ID の順にすべて返す。
func (c *Client) VecgenClients(ctx context.Context) ([]VecgenClient, error) {
	l, err := c.call[clientList[VecgenClient]](ctx, request{method: http.MethodGet, path: []string{"vecgen", "clients"}})
	return l.Items, err
}

// ---- aka-radius ----

// Rule は認可ポリシーのルール（aka-radius §8.2）。上から順に評価し、テナント・NAS-Identifier・SSID がすべて一致した
// 最初のルールを採る。どれにも一致しなければ拒否する。
type Rule struct {
	TenantID int64  `json:"tenantId"`
	NASID    string `json:"nasId"`
	// SSIDs は ["*"]（すべて）か SSID の並び。
	SSIDs []string `json:"ssids"`
	// VLANID と SessionTimeout は 0 なら送らない（付けない）。
	VLANID         int `json:"vlanId,omitzero"`
	SessionTimeout int `json:"sessionTimeout,omitzero"`
}

// RadiusSubscriber は aka-radius の加入者（状態・取得先・認可ポリシーのルール）。
type RadiusSubscriber struct {
	IMSI           string    `json:"imsi"`
	State          string    `json:"state"`
	StateChangedAt time.Time `json:"stateChangedAt"`
	SourceID       string    `json:"sourceId"`
	Rules          []Rule    `json:"rules"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// RadiusSubscriberCreate は aka-radius の加入者の登録の内容（ルールは 1〜32 件）。
type RadiusSubscriberCreate struct {
	IMSI     string `json:"imsi"`
	State    string `json:"state"`
	SourceID string `json:"sourceId"`
	Rules    []Rule `json:"rules"`
}

// RadiusSubscriberUpdate は aka-radius の加入者の変更（JSON Merge Patch。空の項目は送らない。ルールは全体を置き換える）。
type RadiusSubscriberUpdate struct {
	SourceID string `json:"sourceId,omitempty"`
	Rules    []Rule `json:"rules,omitempty"`
}

// RadiusBatch は aka-radius の一括取得の結果（要求の順）。
type RadiusBatch struct {
	Items    []RadiusSubscriber `json:"items"`
	NotFound []string           `json:"notFound"`
}

// CircuitBreaker は取得先の遮断器の状態（auth が報告したもの）。
type CircuitBreaker struct {
	State      string    `json:"state"`
	Since      time.Time `json:"since"`
	ReportedAt time.Time `json:"reportedAt"`
}

// Source は認証ベクターの取得先（aka-radius の取得先の登録簿）。
type Source struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	BaseURL           string          `json:"baseUrl"`
	TimeoutMs         int             `json:"timeoutMs"`
	ClientCertificate *Certificate    `json:"clientCertificate,omitzero"`
	SubscriberCount   int64           `json:"subscriberCount"`
	CircuitBreaker    *CircuitBreaker `json:"circuitBreaker,omitzero"`
}

// Session は aka-radius のセッション。
type Session struct {
	ID               string    `json:"id"`
	IMSI             string    `json:"imsi"`
	TenantID         int64     `json:"tenantId"`
	NASIP            string    `json:"nasIp"`
	NASIdentifier    string    `json:"nasIdentifier"`
	CallingStationID string    `json:"callingStationId"`
	FramedIP         string    `json:"framedIp"`
	AcctSessionID    string    `json:"acctSessionId"`
	StartTime        time.Time `json:"startTime,omitzero"`
	LastUpdate       time.Time `json:"lastUpdate,omitzero"`
	Seconds          int64     `json:"seconds"`
	InOctets         int64     `json:"inOctets"`
	OutOctets        int64     `json:"outOctets"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// SessionParams はセッションの一覧の条件。TenantID か IMSIs のどちらか一方を指定する。
type SessionParams struct {
	// TenantID は刻印のテナント（0 なら指定しない）。
	TenantID int64
	// IMSIs は IMSI（100 件まで）。
	IMSIs []string
	// Cursor は前のページの NextCursor、Limit は 1 ページの件数（1〜1000、省略時は 100）。
	Cursor string
	Limit  int
}

// MaxSessionIMSIs はセッションの一覧で 1 回に指定できる IMSI の数。
const MaxSessionIMSIs = 100

// SessionList はセッションの一覧（期限の早い順）。
type SessionList struct {
	Items      []Session `json:"items"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// RadiusSubscriber は aka-radius の加入者を取得する（なければ 404 USER_NOT_FOUND）。
func (c *Client) RadiusSubscriber(ctx context.Context, imsi string) (RadiusSubscriber, error) {
	return c.call[RadiusSubscriber](ctx, request{method: http.MethodGet, path: []string{"radius", "subscribers", imsi}})
}

// RadiusBatchGet は aka-radius の加入者を一括で取得する（IMSI は 1〜500 件）。
func (c *Client) RadiusBatchGet(ctx context.Context, imsis []string) (RadiusBatch, error) {
	return c.call[RadiusBatch](ctx, request{method: http.MethodPost, path: []string{"radius", "subscribers", "batch-get"},
		body: batchGetRequest{IMSIs: imsis}})
}

// RadiusList は aka-radius の加入者の一覧（IMSI の辞書順）。
type RadiusList struct {
	Items []RadiusSubscriber `json:"items"`
	Total int64              `json:"total"`
	// NextCursor は次のページがあるときの位置。
	NextCursor string `json:"nextCursor,omitempty"`
}

// ListRadiusSubscribers は aka-radius の加入者を IMSI の辞書順に返す（limit は 1〜500。突き合わせに使う）。
func (c *Client) ListRadiusSubscribers(ctx context.Context, cursor string, limit int) (RadiusList, error) {
	q := url.Values{}
	setString(q, "cursor", cursor)
	setInt(q, "limit", int64(limit))
	return c.call[RadiusList](ctx, request{method: http.MethodGet, path: []string{"radius", "subscribers"}, query: q})
}

// CreateRadiusSubscriber は aka-radius に加入者を登録する。
func (c *Client) CreateRadiusSubscriber(ctx context.Context, s RadiusSubscriberCreate, idempotencyKey string) (RadiusSubscriber, error) {
	return c.call[RadiusSubscriber](ctx, request{method: http.MethodPost, path: []string{"radius", "subscribers"},
		body: s, write: true, idempotencyKey: idempotencyKey})
}

// UpdateRadiusSubscriber は aka-radius の加入者を変更する（取得先、ルール）。
func (c *Client) UpdateRadiusSubscriber(ctx context.Context, imsi string, u RadiusSubscriberUpdate, idempotencyKey string) (RadiusSubscriber, error) {
	return c.call[RadiusSubscriber](ctx, request{method: http.MethodPatch, path: []string{"radius", "subscribers", imsi},
		body: u, contentType: mergePatch, write: true, idempotencyKey: idempotencyKey})
}

// RadiusSources は aka-radius の取得先の登録簿をファイルの順に返す。
func (c *Client) RadiusSources(ctx context.Context) ([]Source, error) {
	l, err := c.call[clientList[Source]](ctx, request{method: http.MethodGet, path: []string{"radius", "sources"}})
	return l.Items, err
}

// Sessions は aka-radius のセッションの一覧を返す。
func (c *Client) Sessions(ctx context.Context, p SessionParams) (SessionList, error) {
	q := url.Values{}
	if p.TenantID != 0 {
		q.Set("tenantId", strconv.FormatInt(p.TenantID, 10))
	}
	if len(p.IMSIs) > 0 {
		q.Set("imsi", strings.Join(p.IMSIs, ","))
	}
	setString(q, "cursor", p.Cursor)
	setInt(q, "limit", int64(p.Limit))
	return c.call[SessionList](ctx, request{method: http.MethodGet, path: []string{"radius", "sessions"}, query: q})
}

// ---- 監査ログ ----

// AuditSource は監査ログの元（provisioner 自身と、中継する下流 2 つ）。
type AuditSource string

const (
	AuditProvisioner AuditSource = "provisioner"
	AuditVecgen      AuditSource = "vecgen"
	AuditRadius      AuditSource = "radius"
)

// AuditLogParams は監査ログの一覧の条件。ゼロ値の項目は指定しない。
type AuditLogParams struct {
	// Before は前のページの NextBefore（このエントリID より古いものを返す）。
	Before string
	// Limit は 1 ページの件数（1〜500、省略時は 100）。
	Limit int
}

// AuditLogEntry は provisioner・aka-vecgen・aka-radius の監査ログの 1 件（3 つとも同じ形）。
type AuditLogEntry struct {
	ID         string         `json:"id"`
	Time       time.Time      `json:"time"`
	Operator   string         `json:"operator"`
	MgmtClient string         `json:"mgmtClient"`
	Action     string         `json:"action"`
	Target     string         `json:"target"`
	Detail     map[string]any `json:"detail,omitempty"`
	TraceID    string         `json:"traceId"`
}

// AuditLogList は監査ログの一覧（新しい順）。
type AuditLogList struct {
	Items      []AuditLogEntry `json:"items"`
	NextBefore string          `json:"nextBefore,omitempty"`
}

// AuditLogs は監査ログを新しい順に返す。
func (c *Client) AuditLogs(ctx context.Context, src AuditSource, p AuditLogParams) (AuditLogList, error) {
	path := []string{"audit-logs"}
	if src != AuditProvisioner {
		path = []string{string(src), "audit-logs"}
	}
	q := url.Values{}
	setString(q, "before", p.Before)
	setInt(q, "limit", int64(p.Limit))
	return c.call[AuditLogList](ctx, request{method: http.MethodGet, path: path, query: q})
}

// ---- 補助 ----

// mergePatch は変更（PATCH）の本文の Content-Type（下流は JSON Merge Patch として受ける）。
const mergePatch = "application/merge-patch+json"

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
