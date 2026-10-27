// Package provisionertest は、provisioner.Client を相手にするテストのための mTLS のテスト用サーバーを提供する。
// provisioner と同じく、クライアント証明書を求め、サーバー証明書（自己署名）を検証させる。
package provisionertest

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/certs"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// Discard はログを捨てる Logger。
var Discard = slog.New(slog.DiscardHandler)

// Server はテスト用のサーバーと、クライアントに使った証明書のファイル。
type Server struct {
	*httptest.Server
	// ClientCertFile はクライアント証明書と秘密鍵（1 つの PEM）、ServerCertFile はサーバー証明書。
	ClientCertFile, ServerCertFile string
}

// NewServer は、ハンドラー h を mTLS で提供するサーバーを立て、それにつながるクライアントを返す。
// クライアントのベース URL は <サーバー>/admin/v1。opts の BaseURL・証明書のファイル・Log は上書きする。
func NewServer(t *testing.T, h http.HandlerFunc, opts provisioner.Options) (*provisioner.Client, *Server) {
	t.Helper()
	dir := t.TempDir()

	serverCertPEM, serverKeyPEM, err := certs.SelfSigned("test-server", []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clientCertPEM, clientKeyPEM, err := certs.SelfSignedClient("ossbss-01", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = slog.NewLogLogger(Discard.Handler(), slog.LevelWarn)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequestClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("client certificate required")
			}
			return nil
		},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	opts.BaseURL = srv.URL + "/admin/v1"
	opts.ClientCertFile = write("client.pem", append(clientCertPEM, clientKeyPEM...))
	opts.ClientKeyFile = ""
	opts.ServerCertFile = write("server.pem", serverCertPEM)
	opts.Log = Discard
	c, err := provisioner.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, &Server{Server: srv, ClientCertFile: opts.ClientCertFile, ServerCertFile: opts.ServerCertFile}
}
