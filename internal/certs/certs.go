// Package certs は、provisioner に提示するクライアント証明書の生成と、証明書のフィンガープリントを扱う
// （既存の BFF の internal/certs から、aka-ossbss に要る部分だけを持ってきたもの。ブラウザとの HTTPS は Cloudflare が終端する）。
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Fingerprint は証明書（DER）の SHA-256 を 16進小文字で返す。
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// SelfSigned はサーバー認証用の ECDSA P-256 の鍵と自己署名証明書を生成し、PEM で返す（テストの provisioner に使う）。
// hosts は SAN で、IP アドレスとして解釈できるものは IP、それ以外は DNS 名として入れる。
func SelfSigned(commonName string, hosts []string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	return selfSigned(commonName, hosts, validFor, x509.ExtKeyUsageServerAuth)
}

// SelfSignedClient はクライアント認証用の ECDSA P-256 の鍵と自己署名証明書を生成し、PEM で返す。
// provisioner に提示するaka-ossbss のクライアント証明書に使う（provisioner はフィンガープリントで照合する）。
func SelfSignedClient(commonName string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	return selfSigned(commonName, nil, validFor, x509.ExtKeyUsageClientAuth)
}

func selfSigned(commonName string, hosts []string, validFor time.Duration, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	notBefore := time.Now().Add(-5 * time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
