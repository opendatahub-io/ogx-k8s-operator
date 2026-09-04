/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"crypto/tls"
	"testing"
)

func TestBuildMetricsOptions_NoCertPath(t *testing.T) {
	opts := buildMetricsOptions(":8080", "", nil)

	if opts.BindAddress != ":8080" {
		t.Errorf("BindAddress = %q, want %q", opts.BindAddress, ":8080")
	}
	if opts.SecureServing {
		t.Error("SecureServing must be false when certPath is empty (kube-rbac-proxy mode)")
	}
	if opts.CertDir != "" {
		t.Errorf("CertDir = %q, want empty when certPath is empty", opts.CertDir)
	}
	if opts.FilterProvider != nil {
		t.Error("FilterProvider must be nil when certPath is empty")
	}
}

func TestBuildMetricsOptions_WithCertPath(t *testing.T) {
	opts := buildMetricsOptions(":8443", "/etc/metrics-certs", nil)

	if opts.BindAddress != ":8443" {
		t.Errorf("BindAddress = %q, want %q", opts.BindAddress, ":8443")
	}
	if !opts.SecureServing {
		t.Error("SecureServing must be true when certPath is provided")
	}
	if opts.CertDir != "/etc/metrics-certs" {
		t.Errorf("CertDir = %q, want %q", opts.CertDir, "/etc/metrics-certs")
	}
	if opts.FilterProvider == nil {
		t.Error("FilterProvider must be set when certPath is provided (metrics auth+authz)")
	}
}

func TestBuildMetricsOptions_TLSOptsPassthrough(t *testing.T) {
	called := false
	customOpt := func(_ *tls.Config) { called = true }

	opts := buildMetricsOptions(":8443", "/etc/certs", []func(*tls.Config){customOpt})
	if len(opts.TLSOpts) != 1 {
		t.Fatalf("TLSOpts length = %d, want 1", len(opts.TLSOpts))
	}

	// Invoke the stored opt to confirm it's the one we passed.
	opts.TLSOpts[0](nil)
	if !called {
		t.Error("TLSOpts function was not passed through to the returned options")
	}
}

// TestBuildMetricsOptions_NoTLSOpts verifies nil TLSOpts is preserved (not replaced with an empty slice).
func TestBuildMetricsOptions_NoTLSOpts(t *testing.T) {
	opts := buildMetricsOptions(":8080", "", nil)
	if opts.TLSOpts != nil {
		t.Errorf("TLSOpts = %v, want nil when nil was passed", opts.TLSOpts)
	}
}
