package cmgr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startFakeRegistry is an mTLS server standing in for zot: a private CA, a
// server certificate for 127.0.0.1, and a client certificate laid out under
// a certs.d-shaped directory the registry client loads. It returns the host
// to name as the registry and the requests the handler saw.
func startFakeRegistry(t *testing.T, handler http.HandlerFunc) (string, *[]*http.Request) {
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	sign := func(template, parent *x509.Certificate, pub *ecdsa.PublicKey, signer *ecdsa.PrivateKey) *x509.Certificate {
		der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	now := time.Now()
	caKey := newKey()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test zot CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	ca := sign(caTemplate, caTemplate, &caKey.PublicKey, caKey)
	serverKey := newKey()
	server := sign(&x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "zot.test"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}, ca, &serverKey.PublicKey, caKey)
	clientKey := newKey()
	client := sign(&x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "cmgr"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca, &clientKey.PublicKey, caKey)

	t.Setenv(REGISTRY_CERT_DIR_ENV, writeCertDir(t, map[string][]byte{
		"ca.crt": pemCert(ca.Raw), "client.cert": pemCert(client.Raw), "client.key": pemKey(t, clientKey),
	}))

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	serverTLS, err := tls.X509KeyPair(pemCert(server.Raw), pemKey(t, serverKey))
	if err != nil {
		t.Fatal(err)
	}
	seen := &[]*http.Request{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r)
		handler(w, r)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverTLS},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "https://"), seen
}

// fakeRegistry is startFakeRegistry with a Manager pointed at it: enough for
// the registry client on its own.
func fakeRegistry(t *testing.T, handler http.HandlerFunc) (*Manager, *[]*http.Request) {
	t.Helper()
	host, seen := startFakeRegistry(t, handler)
	return &Manager{log: newLogger(DISABLED), ctx: context.Background(), challengeRegistry: host}, seen
}

// registryTagPresent asks the registry over the daemon's own mTLS client:
// a HEAD on the manifest, 200 present, 404 absent, anything else an error.
func TestRegistryTagPresent(t *testing.T) {
	m, seen := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifests/s1-1111-challenge"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/manifests/s1-2222-challenge"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	repo := m.challengeRegistry + "/cat/chal:"

	present, err := m.registryTagPresent(repo + "s1-1111-challenge")
	if err != nil || !present {
		t.Fatalf("a tag the registry serves: present=%v err=%v", present, err)
	}
	present, err = m.registryTagPresent(repo + "s1-2222-challenge")
	if err != nil || present {
		t.Fatalf("a tag the registry lacks: present=%v err=%v", present, err)
	}
	if _, err := m.registryTagPresent(repo + "s1-3333-challenge"); err == nil {
		t.Fatal("a registry answering 500 was taken for an answer")
	}

	if len(*seen) != 3 {
		t.Fatalf("%d requests reached the registry, want 3", len(*seen))
	}
	first := (*seen)[0]
	if first.Method != http.MethodHead {
		t.Errorf("method %s, want HEAD", first.Method)
	}
	if first.URL.Path != "/v2/cat/chal/manifests/s1-1111-challenge" {
		t.Errorf("path %s", first.URL.Path)
	}
	if !strings.Contains(first.Header.Get("Accept"), "application/vnd.oci.image.manifest.v1+json") {
		t.Errorf("Accept does not name the OCI manifest type: %q", first.Header.Get("Accept"))
	}
	if len(first.TLS.PeerCertificates) == 0 || first.TLS.PeerCertificates[0].Subject.CommonName != "cmgr" {
		t.Error("the request did not carry the client certificate")
	}
}

// A registry that cannot be reached at all is an error, never "absent".
func TestRegistryTagPresentUnreachable(t *testing.T) {
	m, _ := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {})
	// A port that was listening a moment ago and is closed now refuses at
	// once, where a port nothing ever listened on may just drop the
	// connection under some network stacks and run out the whole timeout.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	l.Close()
	m.challengeRegistry = closed
	if present, err := m.registryTagPresent(closed + "/cat/chal:tag"); err == nil || present {
		t.Fatalf("an unreachable registry answered present=%v err=%v", present, err)
	}
}

// registryDeleteTag, over the same client: 202 and 404 are done, 405 is a
// registry that will not delete tags, anything else is the status.
func TestRegistryDeleteTag(t *testing.T) {
	m, seen := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifests/accepted"):
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/manifests/gone"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/manifests/refused"):
			w.WriteHeader(http.StatusMethodNotAllowed)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	repo := m.challengeRegistry + "/cat/chal:"

	if err := m.registryDeleteTag(repo + "accepted"); err != nil {
		t.Errorf("202: %v", err)
	}
	if err := m.registryDeleteTag(repo + "gone"); err != nil {
		t.Errorf("404: %v", err)
	}
	if err := m.registryDeleteTag(repo + "refused"); err == nil || !strings.Contains(err.Error(), "leaked") {
		t.Errorf("405: %v", err)
	}
	if err := m.registryDeleteTag(repo + "broken"); err == nil {
		t.Error("500 was taken for done")
	}
	if len(*seen) != 4 {
		t.Fatalf("%d requests reached the registry, want 4", len(*seen))
	}
	if (*seen)[0].Method != http.MethodDelete {
		t.Errorf("method %s, want DELETE", (*seen)[0].Method)
	}
}
