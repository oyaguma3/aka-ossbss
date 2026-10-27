package certs

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestSelfSigned(t *testing.T) {
	certPEM, keyPEM, err := SelfSignedClient("ossbss-01", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf := pair.Leaf
	if leaf.Subject.CommonName != "ossbss-01" || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth ||
		time.Until(leaf.NotAfter) > time.Hour {
		t.Errorf("client cert = %+v", leaf)
	}
	if fp := Fingerprint(leaf); len(fp) != 64 || fp != Fingerprint(leaf) {
		t.Errorf("fingerprint = %q", fp)
	}

	certPEM, _, err = SelfSigned("server", []string{"127.0.0.1", "aka-provisioner"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.IPAddresses) != 1 || len(c.DNSNames) != 1 || c.DNSNames[0] != "aka-provisioner" || c.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("server cert SAN = %v %v", c.IPAddresses, c.DNSNames)
	}
}
