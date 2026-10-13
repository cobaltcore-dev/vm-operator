// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// userAgent is the User-Agent value set on every outgoing HTTP request made
// through clients created by this package, identifying the vm-operator as the
// caller. It defaults to "vm-operator" and can be overridden once at process
// startup via SetUserAgent (e.g. "vm-operator/sha-70af93a8").
var userAgent = "vm-operator"

// SetUserAgent sets the User-Agent that this package's HTTP clients send. The
// component and version are combined as "component/version"; if version is
// empty, only the component is used. An empty component leaves the default
// ("vm-operator") unchanged.
func SetUserAgent(component, version string) {
	if component == "" {
		return
	}
	if version == "" {
		userAgent = component
		return
	}
	userAgent = component + "/" + version
}

// HTTPClientConfig configures the HTTP client used to talk to OpenStack,
// including an optional client TLS certificate and whether to trust a
// self-signed server certificate.
type HTTPClientConfig struct {
	Cert    string `json:"cert,omitempty"`
	CertKey string `json:"certKey,omitempty"`

	// If the certificate is self-signed, we need to skip verification.
	SelfSigned bool `json:"selfSigned,omitempty"`
}

// requestLogger is an http.RoundTripper that logs each request.
type requestLogger struct {
	T http.RoundTripper
}

// RoundTrip logs the request URL before making the request.
func (lrt *requestLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	slog.Info("making http request", "url", req.URL.String())
	return lrt.T.RoundTrip(req)
}

// userAgentTransport is an http.RoundTripper that sets the vm-operator
// User-Agent on every request, identifying the caller. Clients that build their
// own transport need this, as they don't share http.DefaultTransport.
type userAgentTransport struct {
	T http.RoundTripper
}

// RoundTrip sets the User-Agent header to the vm-operator user agent. The
// request is cloned so the caller's request is not mutated.
func (uat *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", userAgent)
	return uat.T.RoundTrip(req)
}

// WrapUserAgent wraps the given RoundTripper so that every request carries the
// vm-operator User-Agent header. Use it for hand-built transports that neither
// go through NewHTTPClient nor the shared http.DefaultTransport.
func WrapUserAgent(rt http.RoundTripper) http.RoundTripper {
	return &userAgentTransport{T: rt}
}

// NewTransport returns an *http.Transport configured with TLS client
// certificates from the given config. If no certificate is provided, a plain
// *http.Transport is returned.
//
// Both branches set Proxy from the environment: a hand-built http.Transport
// does NOT inherit http.DefaultTransport's proxy handling, so without this the
// client would ignore HTTP(S)_PROXY/NO_PROXY and bypass a proxy the cluster
// relies on to reach OpenStack.
func NewTransport(conf HTTPClientConfig) (*http.Transport, error) {
	if conf.Cert == "" {
		return &http.Transport{Proxy: http.ProxyFromEnvironment}, nil
	}
	if conf.CertKey == "" {
		return nil, errors.New("missing cert key")
	}
	cert, err := tls.X509KeyPair(
		[]byte(conf.Cert),
		[]byte(conf.CertKey),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate: %w", err)
	}
	// The cert is a *client* certificate for mutual TLS, so it goes only in
	// Certificates. We deliberately leave RootCAs nil: a non-nil RootCAs would
	// replace the system root pool, breaking verification of the server's own
	// certificate when SelfSigned is false.
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			// If the server cert is self signed, skip verification.
			//nolint:gosec
			InsecureSkipVerify: conf.SelfSigned,
		},
	}, nil
}

// NewHTTPClient creates a new HTTP client with the given CA cert configuration
// and logging for each request.
func NewHTTPClient(conf HTTPClientConfig) (*http.Client, error) {
	transport, err := NewTransport(conf)
	if err != nil {
		return nil, err
	}
	if conf.Cert == "" {
		slog.Debug("making http requests without custom trusted cert bundle")
	}
	return &http.Client{Transport: &userAgentTransport{T: &requestLogger{T: transport}}}, nil
}
