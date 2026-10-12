// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestNewTransport_noCert(t *testing.T) {
	tr, err := NewTransport(HTTPClientConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.TLSClientConfig != nil {
		t.Error("expected a plain transport with no TLS config when no cert is given")
	}
	// Must honor HTTP(S)_PROXY/NO_PROXY — a hand-built transport otherwise
	// ignores the environment's proxy, which can bypass the route to OpenStack.
	if tr.Proxy == nil {
		t.Error("expected Proxy to be set from the environment")
	}
}

func TestNewTransport_certWithoutKey(t *testing.T) {
	_, err := NewTransport(HTTPClientConfig{Cert: "cert-pem"})
	if err == nil {
		t.Fatal("expected an error when a cert is given without a key")
	}
}

func TestNewTransport_invalidCert(t *testing.T) {
	_, err := NewTransport(HTTPClientConfig{Cert: "not-a-pem", CertKey: "also-not-a-pem"})
	if err == nil {
		t.Fatal("expected an error for an unparsable cert/key pair")
	}
}

func TestNewTransport_validCert(t *testing.T) {
	certPEM, keyPEM := newSelfSignedCertPEM(t)

	for _, selfSigned := range []bool{false, true} {
		t.Run("selfSigned="+strconv.FormatBool(selfSigned), func(t *testing.T) {
			tr, err := NewTransport(HTTPClientConfig{
				Cert:       string(certPEM),
				CertKey:    string(keyPEM),
				SelfSigned: selfSigned,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tr.TLSClientConfig == nil {
				t.Fatal("expected a TLS config when a cert is given")
			}
			if n := len(tr.TLSClientConfig.Certificates); n != 1 {
				t.Fatalf("expected 1 client certificate, got %d", n)
			}
			if tr.TLSClientConfig.RootCAs == nil {
				t.Error("expected the cert to be added to the root CA pool")
			}
			if tr.TLSClientConfig.InsecureSkipVerify != selfSigned {
				t.Errorf("InsecureSkipVerify = %v, want %v", tr.TLSClientConfig.InsecureSkipVerify, selfSigned)
			}
			if tr.Proxy == nil {
				t.Error("expected Proxy to be set from the environment")
			}
		})
	}
}

func TestNewHTTPClient_noCert(t *testing.T) {
	client, err := NewHTTPClient(HTTPClientConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The transport chain must be userAgentTransport -> requestLogger ->
	// *http.Transport so every request is tagged and logged.
	uat, ok := client.Transport.(*userAgentTransport)
	if !ok {
		t.Fatalf("expected outermost transport to be *userAgentTransport, got %T", client.Transport)
	}
	lrt, ok := uat.T.(*requestLogger)
	if !ok {
		t.Fatalf("expected inner transport to be *requestLogger, got %T", uat.T)
	}
	if _, ok := lrt.T.(*http.Transport); !ok {
		t.Errorf("expected base transport to be *http.Transport, got %T", lrt.T)
	}
}

func TestNewHTTPClient_propagatesTransportError(t *testing.T) {
	// A cert without a key makes NewTransport fail; NewHTTPClient must surface
	// that rather than returning a half-built client.
	client, err := NewHTTPClient(HTTPClientConfig{Cert: "cert-pem"})
	if err == nil {
		t.Fatal("expected NewHTTPClient to propagate the transport error")
	}
	if client != nil {
		t.Errorf("expected a nil client on error, got %+v", client)
	}
}

func TestNewHTTPClient_tagsAndSendsRequests(t *testing.T) {
	defer func() { userAgent = "vm-operator" }()
	SetUserAgent("vm-operator", "sha-test")

	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client, err := NewHTTPClient(HTTPClientConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	if gotUA != "vm-operator/sha-test" {
		t.Errorf("server saw User-Agent %q, want %q", gotUA, "vm-operator/sha-test")
	}
}

func TestRequestLogger_RoundTrip_delegates(t *testing.T) {
	var called bool
	lrt := &requestLogger{T: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusTeapot, Body: http.NoBody}, nil
	})}

	req := httptest.NewRequest(http.MethodGet, "https://example.com", http.NoBody)
	resp, err := lrt.RoundTrip(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("requestLogger did not delegate to the wrapped RoundTripper")
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
}

func TestUserAgentTransport_RoundTrip(t *testing.T) {
	defer func() { userAgent = "vm-operator" }()
	SetUserAgent("vm-operator", "sha-xyz")

	var captured string
	uat := &userAgentTransport{T: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = req.Header.Get("User-Agent")
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})}

	req := httptest.NewRequest(http.MethodGet, "https://example.com", http.NoBody)
	if _, err := uat.RoundTrip(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured != "vm-operator/sha-xyz" {
		t.Errorf("wrapped transport saw User-Agent %q, want %q", captured, "vm-operator/sha-xyz")
	}
	// The caller's request must not be mutated.
	if req.Header.Get("User-Agent") != "" {
		t.Error("userAgentTransport mutated the caller's request")
	}
}

func TestWrapUserAgent(t *testing.T) {
	defer func() { userAgent = "vm-operator" }()
	SetUserAgent("vm-operator", "sha-abc123")

	var captured string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = req.Header.Get("User-Agent")
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})

	wrapped := WrapUserAgent(base)
	req := httptest.NewRequest(http.MethodGet, "https://example.com", http.NoBody)
	if _, err := wrapped.RoundTrip(req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured != "vm-operator/sha-abc123" {
		t.Errorf("User-Agent = %q, want %q", captured, "vm-operator/sha-abc123")
	}
	// The caller's request must not be mutated.
	if req.Header.Get("User-Agent") != "" {
		t.Error("WrapUserAgent mutated the caller's request")
	}
}

func TestSetUserAgent(t *testing.T) {
	defer func() { userAgent = "vm-operator" }()

	tests := []struct {
		name               string
		component, version string
		want               string
	}{
		{"component and version", "vm-operator", "sha-1", "vm-operator/sha-1"},
		{"component only", "vm-operator", "", "vm-operator"},
		{"empty component is a no-op", "", "ignored", "sentinel"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			userAgent = "sentinel"
			SetUserAgent(tc.component, tc.version)
			if userAgent != tc.want {
				t.Errorf("userAgent = %q, want %q", userAgent, tc.want)
			}
		})
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// newSelfSignedCertPEM returns a freshly generated self-signed certificate and
// its private key, both PEM-encoded, for exercising the TLS branch of
// NewTransport without touching the filesystem.
func newSelfSignedCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "vm-operator-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
