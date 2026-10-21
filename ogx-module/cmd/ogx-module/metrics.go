package main

import (
	"crypto/tls"
	"fmt"
	"path/filepath"

	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func buildMetricsOptions(addr, certPath string, tlsOpts []func(*tls.Config)) (metricsserver.Options, error) {
	opts := metricsserver.Options{
		BindAddress:   addr,
		SecureServing: true,
		TLSOpts:       tlsOpts,
		CertDir:       certPath,
	}
	if addr != "0" && certPath != "" {
		// controller-runtime otherwise silently substitutes a self-signed
		// certificate when explicitly configured certificate files are absent.
		if _, err := tls.LoadX509KeyPair(filepath.Join(certPath, "tls.crt"), filepath.Join(certPath, "tls.key")); err != nil {
			return opts, fmt.Errorf("failed to load metrics serving certificate: %w", err)
		}
	}
	return opts, nil
}
