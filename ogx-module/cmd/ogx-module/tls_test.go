package main

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	pkgtls "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/tls"
	configv1 "github.com/openshift/api/config/v1"
)

func TestTLSHandshakesInMemory(t *testing.T) {
	dir, roots := metricsTestCertificate(t)
	cert, err := tls.LoadX509KeyPair(dir+"/tls.crt", dir+"/tls.key")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		minimum configv1.TLSProtocolVersion
		version uint16
		allowed bool
	}{
		{"TLS12 permitted", configv1.VersionTLS12, tls.VersionTLS12, true},
		{"TLS11 excluded", configv1.VersionTLS12, tls.VersionTLS11, false},
		{"TLS12 excluded", configv1.VersionTLS13, tls.VersionTLS12, false},
		{"TLS13 permitted", configv1.VersionTLS13, tls.VersionTLS13, true},
		{"custom TLS10", configv1.VersionTLS10, tls.VersionTLS10, true},
		{"custom TLS11", configv1.VersionTLS11, tls.VersionTLS11, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := pkgtls.OptionsFromProfile(configv1.TLSProfileSpec{
				MinTLSVersion: tc.minimum, Ciphers: []string{"ECDHE-ECDSA-AES128-SHA"},
			}, configv1.TLSAdherencePolicyStrictAllComponents, logr.Discard())
			if err != nil {
				t.Fatal(err)
			}
			serverConfig := applyTLSOptions(opts)
			serverConfig.Certificates = []tls.Certificate{cert}
			serverConfig.SessionTicketsDisabled = true
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()
			deadline := time.Now().Add(3 * time.Second)
			serverConn.SetDeadline(deadline)
			clientConn.SetDeadline(deadline)
			done := make(chan error, 1)
			go func() { done <- tls.Server(serverConn, serverConfig).Handshake() }()
			clientTLS := tls.Client(clientConn, &tls.Config{
				RootCAs: roots, ServerName: "localhost", MinVersion: tc.version, MaxVersion: tc.version,
				CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA},
			})
			clientErr := clientTLS.Handshake()
			serverErr := <-done
			if tc.allowed {
				if clientErr != nil || serverErr != nil {
					t.Fatalf("handshake rejected: client=%v server=%v", clientErr, serverErr)
				}
				if clientTLS.ConnectionState().Version != tc.version {
					t.Fatal("wrong TLS version negotiated")
				}
			} else if clientErr == nil || serverErr == nil {
				t.Fatalf("forbidden protocol accepted: client=%v server=%v", clientErr, serverErr)
			}
		})
	}
}

func applyTLSOptions(opts []func(*tls.Config)) *tls.Config {
	cfg := &tls.Config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}
