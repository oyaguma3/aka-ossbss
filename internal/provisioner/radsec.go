package provisioner

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// radsec-ca とエッジの管理API の中継（aka-provisioner の API 2.1.0。/admin/v2/ca/...、/admin/v2/edges/{edgeId}/...）。
// 項目は radsec-ca の docs/openapi/ca-api.yaml と radsec-edge の docs/openapi/edge-api.yaml に合わせる。
// aka-provisioner のタイムアウトが 30 秒なので、呼び出しの上限はそれより長い Options.RadsecTimeout を使う（段階 4-3 の OB17）。

// radsec-ca とエッジの cause。
const (
	CauseEdgeNotFound        = "EDGE_NOT_FOUND"
	CauseIdempotencyConflict = "IDEMPOTENCY_CONFLICT"

	// radsec-ca。
	CauseCAExists           = "CA_EXISTS"
	CauseCANotFound         = "CA_NOT_FOUND"
	CauseCADisabled         = "CA_DISABLED"
	CauseCAFingerprintInUse = "CA_FINGERPRINT_IN_USE"
	CauseCertNotFound       = "CERT_NOT_FOUND"
	CauseCRLNotFound        = "CRL_NOT_FOUND"
	CauseCRLInvalid         = "CRL_INVALID"
	CauseCRLRequired        = "CRL_REQUIRED"
	CauseCSRInvalid         = "CSR_INVALID"
	CauseKeyTypeUnsupported = "KEY_TYPE_UNSUPPORTED"
	CauseNameReserved       = "NAME_RESERVED"
	CauseValidationError    = "VALIDATION_ERROR"
	CauseTenantOnlyNotSet   = "TENANT_ONLY_NOT_CONFIRMED"
	CauseNoCRLNotAccepted   = "NO_CRL_NOT_ACCEPTED"

	// エッジ。
	CauseValidation       = "VALIDATION"
	CauseTenantNotFound   = "TENANT_NOT_FOUND"
	CauseContainerMissing = "CONTAINER_NOT_FOUND"
	CauseTenantBusy       = "TENANT_BUSY"
	CauseTenantStopped    = "TENANT_STOPPED"
	CauseNotReady         = "NOT_READY"
	CauseCapacityExceeded = "CAPACITY_EXCEEDED"
	CauseSlugImmutable    = "SLUG_IMMUTABLE"
	CauseConflict         = "CONFLICT"
)

// 証明書のプロファイル。
const (
	ProfileAPClient     = "ap-client"
	ProfileGroupClient  = "group-client"
	ProfileProbeClient  = "probe-client"
	ProfileTenantServer = "tenant-server"
)

// 失効の理由。
const (
	RevokeUnspecified          = "unspecified"
	RevokeKeyCompromise        = "keyCompromise"
	RevokeSuperseded           = "superseded"
	RevokeCessationOfOperation = "cessationOfOperation"
)

// CRL の取得先。
const (
	CRLSourceURL    = "url"
	CRLSourceMeraki = "meraki"
	CRLSourceUpload = "upload"
	CRLSourceNone   = "none"
)

// EdgeDownstream はエッジの下流の名前（/status の downstreams[].name と ProblemDetails の downstream）を返す。
func EdgeDownstream(id string) Downstream { return Downstream("edge:" + id) }

// ---- radsec-ca ----

// CRLInfo は CA の最新の CRL の情報。
type CRLInfo struct {
	Number     int64     `json:"number"`
	ExtNumber  string    `json:"extNumber,omitempty"`
	ThisUpdate time.Time `json:"thisUpdate"`
	NextUpdate time.Time `json:"nextUpdate"`
}

// CRLState は持ち込みの CA の CRL の取得の状態。
type CRLState struct {
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	OKAt      time.Time `json:"okAt,omitzero"`
	Failures  int       `json:"failures"`
	LastError string    `json:"lastError,omitempty"`
}

// CA は radsec-ca の CA。
type CA struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Tenant       string    `json:"tenant,omitempty"`
	Status       string    `json:"status"`
	Subject      string    `json:"subject"`
	NotAfter     time.Time `json:"notAfter"`
	Certificate  string    `json:"certificate,omitempty"`
	CRL          *CRLInfo  `json:"crl,omitempty"`
	CRLError     string    `json:"crlError,omitempty"`
	Fingerprint  string    `json:"fingerprint"`
	SelfSigned   bool      `json:"selfSigned"`
	HasCRL       bool      `json:"hasCrl"`
	CRLSource    string    `json:"crlSource"`
	CRLURL       string    `json:"crlUrl,omitempty"`
	MerakiOrgID  string    `json:"merakiOrgId,omitempty"`
	MerakiKeySet bool      `json:"merakiKeySet"`
	CRLState     *CRLState `json:"crlState,omitempty"`
	CRLWarning   string    `json:"crlWarning,omitempty"`
}

// CRLSource は持ち込みの CA の CRL の取得先。MerakiAPIKey は秘密の値（ログと監査ログに出さない）。
type CRLSource struct {
	Source       string `json:"source"`
	URL          string `json:"url,omitempty"`
	MerakiOrgID  string `json:"merakiOrgId,omitempty"`
	MerakiAPIKey string `json:"merakiApiKey,omitempty"`
	PEM          string `json:"pem,omitempty"`
}

// BroughtCACreate は持ち込みの CA の登録の本文。
type BroughtCACreate struct {
	Tenant              string    `json:"tenant"`
	Kind                string    `json:"kind"`
	Name                string    `json:"name"`
	CertPEM             string    `json:"certPem"`
	CRL                 CRLSource `json:"crl"`
	TenantOnlyConfirmed bool      `json:"tenantOnlyConfirmed"`
	NoCRLAccepted       bool      `json:"noCrlAccepted,omitzero"`
}

// CRLSourceChange は CRL の取得先の変更の本文。
type CRLSourceChange struct {
	CRL           CRLSource `json:"crl"`
	NoCRLAccepted bool      `json:"noCrlAccepted,omitzero"`
}

// CertIssue は発行の本文。
type CertIssue struct {
	Profile string `json:"profile"`
	Tenant  string `json:"tenant"`
	AP      string `json:"ap,omitempty"`
	Edge    string `json:"edge,omitempty"`
	Group   string `json:"group,omitempty"`
	CSR     string `json:"csr"`
}

// Cert は radsec-ca の証明書。
type Cert struct {
	Serial        string    `json:"serial"`
	CAID          string    `json:"caId"`
	Profile       string    `json:"profile"`
	Tenant        string    `json:"tenant"`
	AP            string    `json:"ap,omitempty"`
	Edge          string    `json:"edge,omitempty"`
	Group         string    `json:"group,omitempty"`
	CN            string    `json:"cn"`
	SANDNS        []string  `json:"sanDns,omitempty"`
	SANURI        string    `json:"sanUri,omitempty"`
	NotBefore     time.Time `json:"notBefore"`
	NotAfter      time.Time `json:"notAfter"`
	Status        string    `json:"status"`
	RevokedAt     time.Time `json:"revokedAt,omitzero"`
	RevokeReason  string    `json:"revokeReason,omitempty"`
	Certificate   string    `json:"certificate"`
	CACertificate string    `json:"caCertificate,omitempty"`
}

// CAStatus は radsec-ca の /status の、画面に要る項目（aka-provisioner の /status の downstreams[].status と同じ）。
type CAStatus struct {
	Service   string `json:"service"`
	Version   string `json:"version"`
	URNPrefix string `json:"urnPrefix"`
	DNSDomain string `json:"dnsDomain"`
	CAs       []CA   `json:"cas"`
}

// CAStatus は radsec-ca の状態を読む。
func (c *Client) CAStatus(ctx context.Context) (CAStatus, error) {
	return c.call[CAStatus](ctx, request{method: http.MethodGet, path: []string{"ca", "status"}, long: true})
}

// CreateTenantCA はテナントのクライアントCA を作る（tenant.<slug>。あれば 409 CA_EXISTS）。
func (c *Client) CreateTenantCA(ctx context.Context, slug, idempotencyKey string) (CA, error) {
	return c.call[CA](ctx, request{method: http.MethodPost, path: []string{"ca", "cas"}, long: true, write: true, idempotencyKey: idempotencyKey,
		body: struct {
			Tenant string `json:"tenant"`
		}{slug}})
}

// CreateBroughtCA は持ち込みの CA を登録する（その場で CRL が取れて確かめられたときだけ成功する）。
func (c *Client) CreateBroughtCA(ctx context.Context, b BroughtCACreate, idempotencyKey string) (CA, error) {
	b.Kind = "brought"
	return c.call[CA](ctx, request{method: http.MethodPost, path: []string{"ca", "cas"}, long: true, write: true, idempotencyKey: idempotencyKey, body: b})
}

// GetCA は CA の詳細を読む。
func (c *Client) GetCA(ctx context.Context, id string) (CA, error) {
	return c.call[CA](ctx, request{method: http.MethodGet, path: []string{"ca", "cas", id}, long: true})
}

// TenantCAs はテナントの CA の一覧を読む。
func (c *Client) TenantCAs(ctx context.Context, slug string) ([]CA, error) {
	type list struct {
		Items []CA `json:"items"`
	}
	out, err := c.call[list](ctx, request{method: http.MethodGet, path: []string{"ca", "cas"}, long: true, query: url.Values{"tenant": {slug}}})
	return out.Items, err
}

// DisableCA は CA を止める（止めていればそのまま返す）。
func (c *Client) DisableCA(ctx context.Context, id, idempotencyKey string) (CA, error) {
	return c.call[CA](ctx, request{method: http.MethodPost, path: []string{"ca", "cas", id, "disable"}, long: true, write: true, idempotencyKey: idempotencyKey})
}

// UploadCRL は取得先が upload の持ち込みの CA に CRL を置く。
func (c *Client) UploadCRL(ctx context.Context, id, pem, idempotencyKey string) (CA, error) {
	return c.call[CA](ctx, request{method: http.MethodPut, path: []string{"ca", "cas", id, "crl"}, long: true, write: true, idempotencyKey: idempotencyKey,
		body: struct {
			PEM string `json:"pem"`
		}{pem}})
}

// RefreshCACRL は取得先が url か meraki の持ち込みの CA の CRL をすぐ取りに行く。
func (c *Client) RefreshCACRL(ctx context.Context, id, idempotencyKey string) (CA, error) {
	return c.call[CA](ctx, request{method: http.MethodPost, path: []string{"ca", "cas", id, "crl:refresh"}, long: true, write: true, idempotencyKey: idempotencyKey})
}

// ChangeCRLSource は持ち込みの CA の CRL の取得先を変える。
func (c *Client) ChangeCRLSource(ctx context.Context, id string, ch CRLSourceChange, idempotencyKey string) (CA, error) {
	return c.call[CA](ctx, request{method: http.MethodPut, path: []string{"ca", "cas", id, "crl-source"}, long: true, write: true, idempotencyKey: idempotencyKey, body: ch})
}

// IssueCert は証明書を発行する。
func (c *Client) IssueCert(ctx context.Context, i CertIssue, idempotencyKey string) (Cert, error) {
	return c.call[Cert](ctx, request{method: http.MethodPost, path: []string{"ca", "certs"}, long: true, write: true, idempotencyKey: idempotencyKey, body: i})
}

// GetCert は証明書を読む。
func (c *Client) GetCert(ctx context.Context, serial string) (Cert, error) {
	return c.call[Cert](ctx, request{method: http.MethodGet, path: []string{"ca", "certs", serial}, long: true})
}

// RevokeCert は証明書を失効させる（失効していればそのまま返す）。
func (c *Client) RevokeCert(ctx context.Context, serial, reason, idempotencyKey string) (Cert, error) {
	return c.call[Cert](ctx, request{method: http.MethodPost, path: []string{"ca", "certs", serial, "revoke"}, long: true, write: true, idempotencyKey: idempotencyKey,
		body: struct {
			Reason string `json:"reason"`
		}{reason}})
}

// ---- エッジ ----

// EdgeClientCA は宣言のクライアントCA。
type EdgeClientCA struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Certificate string `json:"certificate"`
	HasCRL      bool   `json:"hasCrl"`
}

// EdgeMatch は共通のブロックの照合の形。
type EdgeMatch struct {
	Attr    string `json:"attr"`
	Pattern string `json:"pattern"`
}

// EdgeAP は宣言の AP かグループ。
type EdgeAP struct {
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	Purpose      string    `json:"purpose,omitempty"`
	CertSerial   string    `json:"certSerial,omitempty"`
	CertNotAfter time.Time `json:"certNotAfter,omitzero"`
}

// EdgeDeclaration はテナントの望む状態（PUT /tenants/{id} の本文）。
type EdgeDeclaration struct {
	Revision     int64          `json:"revision"`
	Slug         string         `json:"slug"`
	Port         int            `json:"port"`
	AuthUpstream string         `json:"authUpstream"`
	AcctUpstream string         `json:"acctUpstream"`
	ClientCAs    []EdgeClientCA `json:"clientCas"`
	SharedMatch  *EdgeMatch     `json:"sharedMatch,omitempty"`
	SourceCIDRs  []string       `json:"sourceCidrs,omitempty"`
	APs          []EdgeAP       `json:"aps,omitempty"`
}

// EdgeApply はエッジの反映の状態。
type EdgeApply struct {
	State          string    `json:"state"`
	StartedAt      time.Time `json:"startedAt,omitzero"`
	LastResult     string    `json:"lastResult,omitempty"`
	LastError      string    `json:"lastError,omitempty"`
	LastRevision   int64     `json:"lastRevision,omitzero"`
	LastFinishedAt time.Time `json:"lastFinishedAt,omitzero"`
}

// EdgeTenantSummary はエッジのテナントの要約（GET /tenants の 1 件）。
type EdgeTenantSummary struct {
	ID              string    `json:"id"`
	Slug            string    `json:"slug"`
	Port            int       `json:"port"`
	Enabled         bool      `json:"enabled"`
	Revision        int64     `json:"revision"`
	AppliedRevision int64     `json:"appliedRevision"`
	Pending         bool      `json:"pending"`
	ConfigHash      string    `json:"configHash,omitempty"`
	AppliedAt       time.Time `json:"appliedAt,omitzero"`
	Missing         []string  `json:"missing"`
	ProbeProblem    string    `json:"probeProblem,omitempty"`
	Apply           EdgeApply `json:"apply"`
	APCount         int       `json:"apCount"`
}

// EdgeCertInfo はエッジに置いた証明書の情報。
type EdgeCertInfo struct {
	Serial   string    `json:"serial"`
	NotAfter time.Time `json:"notAfter"`
	Subject  string    `json:"subject"`
}

// EdgeTenant はエッジのテナントの詳細（GET /tenants/{id}）。
type EdgeTenant struct {
	EdgeTenantSummary
	ServerCert *EdgeCertInfo `json:"serverCert,omitempty"`
	ProbeCert  *EdgeCertInfo `json:"probeCert,omitempty"`
}

// EdgeCRLState は CA ごとの CRL の状態。
type EdgeCRLState struct {
	CAID      string `json:"caId"`
	OK        bool   `json:"ok"`
	LastError string `json:"lastError,omitempty"`
}

// EdgeCRLRefresh は CRL の取り直しの結果。
type EdgeCRLRefresh struct {
	OK    bool           `json:"ok"`
	Error string         `json:"error,omitempty"`
	Items []EdgeCRLState `json:"items"`
}

// EdgeSeenAP は AP・グループの接続の状態。
type EdgeSeenAP struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Connected  bool      `json:"connected"`
	LastUp     time.Time `json:"lastUp,omitzero"`
	LastDown   time.Time `json:"lastDown,omitzero"`
	LastIP     string    `json:"lastIp,omitempty"`
	LastReject time.Time `json:"lastReject,omitzero"`
}

// EdgeSeenShared は共通のブロックの送信元の IP ごとの接続の状態。
type EdgeSeenShared struct {
	IP          string    `json:"ip"`
	Connections int       `json:"connections"`
	LastUp      time.Time `json:"lastUp,omitzero"`
	LastDown    time.Time `json:"lastDown,omitzero"`
}

// EdgeLastSeen はテナントの接続の状態。
type EdgeLastSeen struct {
	APs    []EdgeSeenAP     `json:"aps"`
	Shared []EdgeSeenShared `json:"shared"`
}

func edgeTenantPath(edge string, tenant int64, rest ...string) []string {
	return append([]string{"edges", edge, "tenants", strconv.FormatInt(tenant, 10)}, rest...)
}

// EdgeTenants はエッジの全テナントの要約を読む（中央の整合のループはエッジごとにこれを 1 回読む）。
func (c *Client) EdgeTenants(ctx context.Context, edge string) ([]EdgeTenantSummary, error) {
	type list struct {
		Items []EdgeTenantSummary `json:"items"`
	}
	out, err := c.call[list](ctx, request{method: http.MethodGet, path: []string{"edges", edge, "tenants"}, long: true})
	return out.Items, err
}

// EdgeTenant はエッジのテナントの詳細を読む。
func (c *Client) EdgeTenant(ctx context.Context, edge string, tenant int64) (EdgeTenant, error) {
	return c.call[EdgeTenant](ctx, request{method: http.MethodGet, path: edgeTenantPath(edge, tenant), long: true})
}

// PutEdgeTenant はテナントの望む状態を送る（なければ作る）。
func (c *Client) PutEdgeTenant(ctx context.Context, edge string, tenant int64, d EdgeDeclaration, idempotencyKey string) (EdgeTenant, error) {
	return c.call[EdgeTenant](ctx, request{method: http.MethodPut, path: edgeTenantPath(edge, tenant), long: true, write: true, idempotencyKey: idempotencyKey, body: d})
}

// DeleteEdgeTenant はエッジからテナントを消す。
func (c *Client) DeleteEdgeTenant(ctx context.Context, edge string, tenant int64, idempotencyKey string) error {
	_, err := c.call[struct{}](ctx, request{method: http.MethodDelete, path: edgeTenantPath(edge, tenant), long: true, write: true, idempotencyKey: idempotencyKey})
	return err
}

type edgeCSR struct {
	CSR string `json:"csr"`
}

type edgeCertificate struct {
	Certificate string `json:"certificate"`
}

// EdgeServerCSR はテナントのサーバー証明書の鍵を作り、CSR を返す。
func (c *Client) EdgeServerCSR(ctx context.Context, edge string, tenant int64, idempotencyKey string) (string, error) {
	out, err := c.call[edgeCSR](ctx, request{method: http.MethodPost, path: edgeTenantPath(edge, tenant, "server-csr"), long: true, write: true, idempotencyKey: idempotencyKey})
	return out.CSR, err
}

// PutEdgeServerCert は署名済みのサーバー証明書を置く。
func (c *Client) PutEdgeServerCert(ctx context.Context, edge string, tenant int64, certificate, idempotencyKey string) (EdgeTenant, error) {
	return c.call[EdgeTenant](ctx, request{method: http.MethodPut, path: edgeTenantPath(edge, tenant, "server-cert"), long: true, write: true, idempotencyKey: idempotencyKey,
		body: edgeCertificate{certificate}})
}

// EdgeProbeCSR はヘルスチェック用の証明書の鍵を作り、CSR を返す。
func (c *Client) EdgeProbeCSR(ctx context.Context, edge string, tenant int64, idempotencyKey string) (string, error) {
	out, err := c.call[edgeCSR](ctx, request{method: http.MethodPost, path: edgeTenantPath(edge, tenant, "probe-csr"), long: true, write: true, idempotencyKey: idempotencyKey})
	return out.CSR, err
}

// PutEdgeProbeCert は署名済みのヘルスチェック用の証明書を置く。
func (c *Client) PutEdgeProbeCert(ctx context.Context, edge string, tenant int64, certificate, idempotencyKey string) (EdgeTenant, error) {
	return c.call[EdgeTenant](ctx, request{method: http.MethodPut, path: edgeTenantPath(edge, tenant, "probe-cert"), long: true, write: true, idempotencyKey: idempotencyKey,
		body: edgeCertificate{certificate}})
}

// ApplyEdgeTenant は反映を裏で始める。実行のたびに新しいキーを使う（同じキーだと最初の応答を返すだけで何もしない）。
func (c *Client) ApplyEdgeTenant(ctx context.Context, edge string, tenant int64, idempotencyKey string) (EdgeApply, error) {
	return c.call[EdgeApply](ctx, request{method: http.MethodPost, path: edgeTenantPath(edge, tenant, "apply"), long: true, write: true, idempotencyKey: idempotencyKey})
}

// RefreshEdgeCRL は CRL をすぐ取りに行く。実行のたびに新しいキーを使う。
func (c *Client) RefreshEdgeCRL(ctx context.Context, edge string, tenant int64, idempotencyKey string) (EdgeCRLRefresh, error) {
	return c.call[EdgeCRLRefresh](ctx, request{method: http.MethodPost, path: edgeTenantPath(edge, tenant, "crl:refresh"), long: true, write: true, idempotencyKey: idempotencyKey})
}

// RestartEdgeTenant はテナントのコンテナを再起動する。実行のたびに新しいキーを使う。
func (c *Client) RestartEdgeTenant(ctx context.Context, edge string, tenant int64, idempotencyKey string) error {
	_, err := c.call[struct{}](ctx, request{method: http.MethodPost, path: edgeTenantPath(edge, tenant, "restart"), long: true, write: true, idempotencyKey: idempotencyKey})
	return err
}

// EdgeTenantLogs は radsecproxy のログの末尾を読む。
func (c *Client) EdgeTenantLogs(ctx context.Context, edge string, tenant int64, lines int) (string, error) {
	type logs struct {
		Logs string `json:"logs"`
	}
	out, err := c.call[logs](ctx, request{method: http.MethodGet, path: edgeTenantPath(edge, tenant, "logs"), long: true, query: url.Values{"lines": {strconv.Itoa(lines)}}})
	return out.Logs, err
}

// EdgeTenantLastSeen はテナントの接続の状態を読む。
func (c *Client) EdgeTenantLastSeen(ctx context.Context, edge string, tenant int64) (EdgeLastSeen, error) {
	return c.call[EdgeLastSeen](ctx, request{method: http.MethodGet, path: edgeTenantPath(edge, tenant, "last-seen"), long: true})
}
