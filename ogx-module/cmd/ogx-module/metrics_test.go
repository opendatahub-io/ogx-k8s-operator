package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	pkgtls "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/tls"
	configv1 "github.com/openshift/api/config/v1"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestMetricsCertificates(t *testing.T) {
	dir, _ := metricsTestCertificate(t)
	for _, tc := range []struct {
		name, addr, path string
		wantErr          bool
	}{
		{"self-signed", ":8443", "", false},
		{"mounted", ":8443", dir, false},
		{"missing", ":8443", t.TempDir(), true},
		{"disabled", "0", "/missing-certificate", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := buildMetricsOptions(tc.addr, tc.path, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if !opts.SecureServing {
				t.Fatal("metrics must never use cleartext HTTP")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("invalid key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildMetricsOptions(":8443", dir, nil); err == nil {
		t.Fatal("invalid certificate accepted")
	}
}

func TestMetricsTLSHandshakes(t *testing.T) {
	for _, profileType := range []configv1.TLSProfileType{configv1.TLSProfileIntermediateType, configv1.TLSProfileModernType} {
		t.Run(string(profileType), func(t *testing.T) {
			dir, roots := metricsTestCertificate(t)
			tlsOpts, err := pkgtls.OptionsFromProfile(*configv1.TLSProfiles[profileType], configv1.TLSAdherencePolicyStrictAllComponents, logr.Discard())
			if err != nil {
				t.Fatal(err)
			}
			opts, err := buildMetricsOptions("127.0.0.1:0", dir, tlsOpts)
			if err != nil {
				t.Fatal(err)
			}
			addr := startMetricsServer(t, opts)
			for _, version := range []uint16{tls.VersionTLS11, tls.VersionTLS12, tls.VersionTLS13} {
				cfg := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: version, MaxVersion: version}
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, cfg)
				allowed := version >= applyTLSOptions(tlsOpts).MinVersion
				if !allowed {
					if err == nil {
						conn.Close()
						t.Fatalf("forbidden TLS version %x negotiated", version)
					}
					continue
				}
				if err != nil {
					t.Fatalf("TLS %x rejected: %v", version, err)
				}
				if conn.ConnectionState().Version != version {
					t.Fatal("wrong protocol negotiated")
				}
				conn.Close()
			}

			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost"}}
			defer transport.CloseIdleConnections()
			httpsClient := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			resp, err := httpsClient.Get("https://" + addr + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "go_goroutines") {
				t.Fatalf("HTTPS metrics request failed: status=%d error=%v", resp.StatusCode, err)
			}
			httpClient := &http.Client{Timeout: 3 * time.Second}
			resp, err = httpClient.Get("http://" + addr + "/metrics")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode < 400 {
					t.Fatalf("HTTP returned success: %d", resp.StatusCode)
				}
			}
		})
	}
}

func TestMetricsCipherRestriction(t *testing.T) {
	dir, roots := metricsTestCertificate(t)
	tlsOpts, err := pkgtls.OptionsFromProfile(configv1.TLSProfileSpec{
		MinTLSVersion: configv1.VersionTLS12,
		Ciphers:       []string{"TLS_AES_128_GCM_SHA256", "ECDHE-ECDSA-AES128-GCM-SHA256", "not-a-cipher"},
	}, configv1.TLSAdherencePolicyStrictAllComponents, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := buildMetricsOptions("127.0.0.1:0", dir, tlsOpts)
	if err != nil {
		t.Fatal(err)
	}
	addr := startMetricsServer(t, opts)
	for _, suite := range []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384} {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, &tls.Config{
			RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
			CipherSuites: []uint16{suite},
		})
		if suite == tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 {
			if err != nil {
				t.Fatalf("allowed suite rejected: %v", err)
			}
			conn.Close()
		} else if err == nil {
			conn.Close()
			t.Fatal("excluded cipher suite was accepted")
		}
	}
}

func TestSelfSignedMetrics(t *testing.T) {
	tlsOpts, err := pkgtls.OptionsFromProfile(*configv1.TLSProfiles[configv1.TLSProfileIntermediateType], "", logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := buildMetricsOptions("127.0.0.1:0", "", tlsOpts)
	if err != nil {
		t.Fatal(err)
	}
	addr := startMetricsServer(t, opts)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, &tls.Config{
		InsecureSkipVerify: true, // Only test encryption with the generated self-signed certificate.
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func startMetricsServer(t *testing.T, opts metricsserver.Options) string {
	t.Helper()
	server, err := metricsserver.NewServer(opts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var serveErr error
	go func() { serveErr = server.Start(ctx); close(finished) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
			if serveErr != nil {
				t.Errorf("metrics server: %v", serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("metrics server did not stop")
		}
	})
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-finished:
			t.Fatalf("metrics server exited: %v", serveErr)
		case <-deadline:
			t.Fatal("metrics server did not bind")
		case <-ticker.C:
			if addr := server.(interface{ GetBindAddr() string }).GetBindAddr(); addr != "" {
				return addr
			}
		}
	}
}

func metricsTestCertificate(t *testing.T) (string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return dir, roots
}
