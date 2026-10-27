// Package provisioner は aka-provisioner（以下「provisioner」）の API のクライアント（設計概要 §10）。
// API 仕様は aka-provisioner のリポジトリの docs/openapi/provisioner-api.yaml（0.3.0）を参照。
// 既存の BFF（web-gui-for-eapaka-radius）の internal/provapi と internal/pvapi を、1 つにまとめて手直ししたもの
// （本BFF の接続先は provisioner だけなので、本PoCの Provisioning API に直接つなぐための部分は持たない）。
//
// 接続は mTLS で、本BFF のクライアント証明書を提示し、provisioner のサーバー証明書（自己署名）を
// 信頼する証明書として検証する（ホスト名も検証する）。
//
// 変更操作と秘密の値（Ki / OPc、共有シークレット）の取得には操作者が要る。本BFF は利用者のメールアドレスを渡す
// （設計概要 §15 の 6）。WithOperator でコンテキストに入れておくと X-Operator-Id ヘッダーで渡す。入っていなければ
// ErrNoOperator を返し、リクエストを送らない。
//
// トレースID（internal/trace）がコンテキストに入っていれば X-Trace-ID ヘッダーで渡す。入っていなければ呼び出しごとに採番する。
// provisioner はそれを下流（本PoCの provisioning-api、aka-vecgen）にも渡すので、監査ログを突き合わせられる。
package provisioner

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

const (
	// defaultTimeout は 1 回の呼び出しの上限の既定。provisioner の加入者の登録は、補償を含めて下流を何度か呼ぶので長めにする。
	defaultTimeout = 60 * time.Second
	// maxResponseBytes は応答ボディを読む上限。一覧の最大（500 件）でも収まる大きさにする。
	maxResponseBytes     = 16 << 20
	operatorHeader       = "X-Operator-Id"
	traceHeader          = "X-Trace-ID"
	idempotencyKeyHeader = "Idempotency-Key"
	// name はログとエラーに出す API の名前。
	name = "provisioner"
)

// operatorPattern は X-Operator-Id の形式。provisioner 側の検証と同じ。
var operatorPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

var (
	// ErrNoOperator は、操作者が必要な操作でコンテキストに操作者が入っていないことを表す。
	ErrNoOperator = errors.New("provisioner: operator is required for this operation")
	// ErrInvalidOperator は、操作者が X-Operator-Id の形式に合わないことを表す。
	ErrInvalidOperator = errors.New("provisioner: operator does not match " + operatorPattern.String())
)

// Options はクライアントの設定。
type Options struct {
	// BaseURL は provisioner のベース URL（例: https://aka-provisioner:9446/admin/v1）。
	BaseURL string
	// ClientCertFile は本BFF のクライアント証明書（PEM）。
	ClientCertFile string
	// ClientKeyFile はクライアント証明書の秘密鍵（PEM）。空なら ClientCertFile から読む
	// （gen-client-cert は、出力先を分けなければ証明書と秘密鍵を 1 つの PEM にまとめて出力する）。
	ClientKeyFile string
	// ServerCertFile は provisioner のサーバー証明書（PEM）。これを信頼する証明書として検証する。
	ServerCertFile string
	// Timeout は 1 回の呼び出しの上限時間。0 なら 60 秒。
	Timeout time.Duration
	Log     *slog.Logger
}

// Client は provisioner のクライアント。
type Client struct {
	base *url.URL
	hc   *http.Client
	log  *slog.Logger
	// clientCert は提示するクライアント証明書。フィンガープリントの表示に使う。
	clientCert *x509.Certificate
}

// New はクライアントを作る。証明書のファイルはここで読み込む。
func New(opts Options) (*Client, error) {
	base, err := url.Parse(opts.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("provisioner url %q: want https://host:port/path", opts.BaseURL)
	}
	cert, err := tls.LoadX509KeyPair(opts.ClientCertFile, cmp.Or(opts.ClientKeyFile, opts.ClientCertFile))
	if err != nil {
		return nil, fmt.Errorf("load provisioner client certificate: %w", err)
	}
	serverPEM, err := os.ReadFile(opts.ServerCertFile)
	if err != nil {
		return nil, fmt.Errorf("load provisioner server certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(serverPEM) {
		return nil, fmt.Errorf("load provisioner server certificate: no PEM certificate in %s", opts.ServerCertFile)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			RootCAs:      roots,
		},
		// 接続の確立は 10 秒で打ち切る（接続できない相手を、呼び出し全体の上限まで待たない）。
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 8,
	}
	return &Client{
		base: base,
		hc: &http.Client{
			Transport: tr,
			Timeout:   cmp.Or(opts.Timeout, defaultTimeout),
			// provisioner はリダイレクトを返さない。返ってきたらそのまま応答として扱う。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		log:        opts.Log,
		clientCert: cert.Leaf,
	}, nil
}

// ClientCertificate は provisioner に提示するクライアント証明書を返す。
func (c *Client) ClientCertificate() *x509.Certificate { return c.clientCert }

// BaseURL は provisioner のベース URL を返す。
func (c *Client) BaseURL() string { return c.base.String() }

// ---- 操作者 ----

type operatorKey struct{}

// WithOperator は、provisioner に渡す操作者（利用者のメールアドレス）をコンテキストに入れる。
func WithOperator(ctx context.Context, operator string) context.Context {
	return context.WithValue(ctx, operatorKey{}, operator)
}

// OperatorFrom はコンテキストに入れた操作者を返す。
func OperatorFrom(ctx context.Context) string {
	id, _ := ctx.Value(operatorKey{}).(string)
	return id
}

// ---- エラー ----

// ProblemDetails の cause の値。
const (
	CauseInvalidMsgFormat     = "INVALID_MSG_FORMAT"
	CauseInvalidQueryParam    = "INVALID_QUERY_PARAM"
	CauseMandatoryIEMissing   = "MANDATORY_IE_MISSING"
	CauseMandatoryIEIncorrect = "MANDATORY_IE_INCORRECT"
	CauseOptionalIEIncorrect  = "OPTIONAL_IE_INCORRECT"
	CauseUserNotFound         = "USER_NOT_FOUND"
	CauseClientNotFound       = "CLIENT_NOT_FOUND"
	CausePolicyNotFound       = "POLICY_NOT_FOUND"
	CauseSubscriberExists     = "SUBSCRIBER_ALREADY_EXISTS"
	CauseClientExists         = "CLIENT_ALREADY_EXISTS"
	CauseSystemFailure        = "SYSTEM_FAILURE"

	// 以下は provisioner だけの cause。
	CauseKeyStoreMismatch        = "KEY_STORE_MISMATCH"
	CauseOperationInProgress     = "OPERATION_IN_PROGRESS"
	CauseOperationUnresolved     = "OPERATION_UNRESOLVED"
	CauseOperationIncomplete     = "OPERATION_INCOMPLETE"
	CauseOperationNotFound       = "OPERATION_NOT_FOUND"
	CauseOperationStateConflict  = "OPERATION_STATE_CONFLICT"
	CauseIdempotencyKeyMismatch  = "IDEMPOTENCY_KEY_MISMATCH"
	CauseDownstreamError         = "DOWNSTREAM_ERROR"
	CauseDownstreamUnavailable   = "DOWNSTREAM_UNAVAILABLE"
	CauseDownstreamNotConfigured = "DOWNSTREAM_NOT_CONFIGURED"
)

// InvalidParam は不正だった項目。
type InvalidParam struct {
	// Param は項目の名前（例: ki、rules[0].allowedSsids[1]）。
	Param  string `json:"param"`
	Reason string `json:"reason,omitempty"`
}

// Problem は provisioner のエラー応答（ProblemDetails）。下流の作法の cause / invalidParams に、
// provisioner の拡張項目（Downstream 以降）を加えたもの。中継した操作では、下流のエラーがそのまま入る。
type Problem struct {
	Title         string         `json:"title,omitempty"`
	Status        int            `json:"status,omitzero"`
	Detail        string         `json:"detail,omitempty"`
	Cause         string         `json:"cause,omitempty"`
	InvalidParams []InvalidParam `json:"invalidParams,omitempty"`

	// Downstream はエラーの元の下流（prov / aka）。
	Downstream string `json:"downstream,omitempty"`
	// DownstreamStatus と DownstreamCause は下流の HTTP ステータスと cause。
	DownstreamStatus int    `json:"downstreamStatus,omitzero"`
	DownstreamCause  string `json:"downstreamCause,omitempty"`
	// OperationID は操作の記録の ID（書き込みの途中の失敗、または同じ IMSI の未完了の操作）。
	OperationID string `json:"operationId,omitempty"`
	// RolledBack は、補償で元に戻したか（OperationID がある場合だけ）。
	RolledBack *bool `json:"rolledBack,omitempty"`
	// Conflicts は、SUBSCRIBER_ALREADY_EXISTS で同じ IMSI のものがあった場所（poc / aka / policy）。
	Conflicts []string `json:"conflicts,omitempty"`
}

// Error は provisioner がエラーを返したことを表す。
// 応答が ProblemDetails でなかった場合、Problem は空になる。
type Error struct {
	Method string
	Path   string
	Status int
	// TraceID は provisioner が応答の X-Trace-ID で返したトレースID。
	TraceID string
	Problem Problem
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s %s %s: %d %s", name, e.Method, e.Path, e.Status, http.StatusText(e.Status))
	if e.Problem.Cause != "" {
		msg += " (" + e.Problem.Cause + ")"
	}
	if e.Problem.Detail != "" {
		msg += ": " + e.Problem.Detail
	}
	return msg
}

// CauseOf は、err が provisioner のエラー応答なら cause を返す。そうでなければ空文字列。
func CauseOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Problem.Cause
	}
	return ""
}

// IsUnavailable は、err が provisioner に届かなかった（接続・TLS・タイムアウトなど）ことを表すかを返す。
// provisioner が応答を返した場合と、呼び出し側の誤り（操作者の指定漏れ、パスの値の誤りなど）は false。
func IsUnavailable(err error) bool {
	if err == nil || errors.Is(err, ErrNoOperator) || errors.Is(err, ErrInvalidOperator) || errors.Is(err, ErrInvalidPath) {
		return false
	}
	_, isAPIError := errors.AsType[*Error](err)
	return !isAPIError
}

// ---- 呼び出し ----

// request は 1 回の呼び出しの内容。
type request struct {
	method string
	// path は BaseURL からの相対パスの要素。各要素はエスケープして連結する。
	path  []string
	query url.Values
	// body は JSON にして送る値。nil なら送らない。
	body        any
	contentType string
	// needOperator が true なら、コンテキストに操作者が入っていなければ送らない。
	needOperator bool
	// idempotencyKey は Idempotency-Key ヘッダーの値。空なら付けない。
	idempotencyKey string
}

// call は provisioner を呼び出し、2xx の応答ボディを T として読む。T が struct{} ならボディは読まない。
// 2xx 以外の応答は *Error を返す。
func (c *Client) call[T any](ctx context.Context, req request) (T, error) {
	var out T
	resp, err := c.send(ctx, req)
	if err != nil {
		return out, err
	}
	defer drainClose(resp.Body)

	if _, noBody := any(out).(struct{}); noBody {
		return out, nil
	}
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), &out); err != nil {
		return out, fmt.Errorf("%s %s %s: decode response: %w", name, req.method, resp.Request.URL.Path, err)
	}
	return out, nil
}

// send はリクエストを送り、2xx の応答を返す。それ以外の応答は *Error にして返す。
func (c *Client) send(ctx context.Context, req request) (*http.Response, error) {
	op := OperatorFrom(ctx)
	switch {
	case op != "" && !operatorPattern.MatchString(op):
		return nil, ErrInvalidOperator
	case op == "" && req.needOperator:
		return nil, ErrNoOperator
	}
	traceID := cmp.Or(trace.From(ctx), trace.New())

	u, err := c.url(req.path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = req.query.Encode()

	var body io.Reader
	if req.body != nil {
		b, err := json.Marshal(req.body)
		if err != nil {
			return nil, fmt.Errorf("%s %s %s: encode request: %w", name, req.method, u.Path, err)
		}
		body = bytes.NewReader(b)
	}
	hreq, err := http.NewRequestWithContext(ctx, req.method, u.String(), body)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Accept", "application/json, application/problem+json")
	if req.body != nil {
		hreq.Header.Set("Content-Type", cmp.Or(req.contentType, "application/json"))
	}
	if op != "" {
		hreq.Header.Set(operatorHeader, op)
	}
	hreq.Header.Set(traceHeader, traceID)
	if req.idempotencyKey != "" {
		hreq.Header.Set(idempotencyKeyHeader, req.idempotencyKey)
	}

	start := time.Now()
	resp, err := c.hc.Do(hreq)
	if err != nil {
		c.log.Warn("provisioner call failed", "method", req.method, "path", u.Path,
			"trace_id", traceID, "error", err)
		return nil, fmt.Errorf("%s %s %s: %w", name, req.method, u.Path, err)
	}
	// provisioner は使ったトレースID を応答で返す（送った値と同じになるはず）。
	traceID = cmp.Or(resp.Header.Get(traceHeader), traceID)
	// リクエストとレスポンスのボディ、クエリ文字列は出さない（Ki / OPc や検索条件を含みうるため）。
	c.log.Debug("provisioner call", "method", req.method, "path", u.Path, "status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(), "operator", op, "trace_id", traceID)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer drainClose(resp.Body)
	apiErr := &Error{Method: req.method, Path: u.Path, Status: resp.StatusCode, TraceID: traceID}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/problem+json" {
		// 読めなくても、ステータスコードだけで扱えるようにする。
		_ = json.UnmarshalRead(io.LimitReader(resp.Body, maxResponseBytes), &apiErr.Problem)
	}
	return nil, apiErr
}

// ErrInvalidPath は、パスの値が空・"."・".." であることを表す（送らない）。
var ErrInvalidPath = errors.New("provisioner: invalid path element")

// url は BaseURL に path の要素を連結した URL を返す。各要素は 1 つのセグメントとしてエスケープする
// （provisioner の internal/downstream と同じ作り）。url.URL.JoinPath は要素の中の "/" をエスケープしないので使わない
// （例えば IMSI の代わりに "1/secret" が来ても、別の操作のパスとして届かないようにする）。空・"."・".." の要素は、
// provisioner のルーターがパスを正規化して別のパスとして扱いうるので、送らずにエラーにする。
func (c *Client) url(path []string) (*url.URL, error) {
	u := *c.base
	raw := strings.TrimSuffix(c.base.EscapedPath(), "/")
	for _, p := range path {
		if p == "" || p == "." || p == ".." {
			return nil, fmt.Errorf("%w %q", ErrInvalidPath, p)
		}
		raw += "/" + url.PathEscape(p)
	}
	u.RawPath = raw
	u.Path, _ = url.PathUnescape(raw) // PathEscape したものなので失敗しない
	return &u, nil
}

// drainClose は、接続を再利用できるよう残りのボディを読み捨ててから閉じる。
func drainClose(body io.ReadCloser) {
	io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes))
	body.Close()
}
