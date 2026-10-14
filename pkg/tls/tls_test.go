/*
Copyright 2025.

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

package tls

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	restclient "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var testScheme = func() *runtime.Scheme {
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		panic(err)
	}
	return scheme
}()

const (
	apiGroupListPath          = "/apis"
	configAPIResourceListPath = "/apis/config.openshift.io/v1"
)

// applyTLSOpts applies TLS opts to a fresh config the way the manager servers do.
func applyTLSOpts(t *testing.T, opts []func(*tls.Config)) *tls.Config {
	t.Helper()
	cfg := &tls.Config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

func TestFetchAdherence_DeniedReadFails(t *testing.T) {
	denied := map[string]error{
		"forbidden": apierrors.NewForbidden(
			schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("denied")),
		"unauthorized": apierrors.NewUnauthorized("denied"),
	}
	for name, readErr := range denied {
		t.Run(name, func(t *testing.T) {
			k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
					return readErr
				},
			}).Build()

			if _, err := fetchAdherence(context.Background(), k8sClient, logr.Discard()); err == nil {
				t.Error("expected denied adherence read to fail startup, got nil error")
			}
		})
	}
}

func TestFetchAdherence_NonFatalReads(t *testing.T) {
	nonFatal := map[string]error{
		"not found": apierrors.NewNotFound(
			schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster"),
		"no api match": &apimeta.NoResourceMatchError{
			PartialResource: schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "apiservers"}},
		"service unavailable": apierrors.NewServiceUnavailable("temporarily unavailable"),
		"timeout":             apierrors.NewTimeoutError("timeout", 1),
		"wrapped deadline":    fmt.Errorf("failed to request: %w", context.DeadlineExceeded),
		"transport timeout": &url.Error{
			Op: "Get", URL: "https://api.example.test/apiservers/cluster",
			Err: &net.DNSError{IsTimeout: true, Err: "i/o timeout", Name: "api.example.test"},
		},
	}
	for name, readErr := range nonFatal {
		t.Run(name, func(t *testing.T) {
			k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
					return readErr
				},
			}).Build()

			adherence, err := fetchAdherence(context.Background(), k8sClient, logr.Discard())
			if err != nil {
				t.Fatalf("expected non-fatal read to keep going, got error: %v", err)
			}
			if adherence != configv1.TLSAdherencePolicyNoOpinion {
				t.Errorf("adherence = %q, want NoOpinion", adherence)
			}
		})
	}
}

func TestFetchAdherence_Success(t *testing.T) {
	apiServer := &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       configv1.APIServerSpec{TLSAdherence: configv1.TLSAdherencePolicyStrictAllComponents},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(apiServer).Build()

	adherence, err := fetchAdherence(context.Background(), k8sClient, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if adherence != configv1.TLSAdherencePolicyStrictAllComponents {
		t.Errorf("adherence = %q, want %q", adherence, configv1.TLSAdherencePolicyStrictAllComponents)
	}
}

func newResolveTestServer(t *testing.T, adherenceStatus int) *httptest.Server {
	t.Helper()
	var statusBody string
	switch adherenceStatus {
	case http.StatusForbidden:
		statusBody = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"denied","reason":"Forbidden","code":403}`
	case http.StatusServiceUnavailable:
		statusBody = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"temporarily unavailable","reason":"ServiceUnavailable","code":503}`
	default:
		t.Fatalf("unsupported adherence status %d", adherenceStatus)
	}

	apiServerReads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/apiservers/cluster"):
			apiServerReads++
			if apiServerReads == 1 {
				w.Write([]byte(`{"apiVersion":"config.openshift.io/v1","kind":"APIServer","metadata":{"name":"cluster"},"spec":{"tlsSecurityProfile":{"type":"Intermediate"}}}`))
				return
			}
			w.WriteHeader(adherenceStatus)
			w.Write([]byte(statusBody))
		case r.URL.Path == apiGroupListPath:
			w.Write([]byte(`{
				"kind":"APIGroupList",
				"apiVersion":"v1",
				"groups":[{
					"name":"config.openshift.io",
					"versions":[{"groupVersion":"config.openshift.io/v1","version":"v1"}],
					"preferredVersion":{"groupVersion":"config.openshift.io/v1","version":"v1"}
				}]
			}`))
		case r.URL.Path == configAPIResourceListPath:
			w.Write([]byte(`{
				"kind":"APIResourceList",
				"groupVersion":"config.openshift.io/v1",
				"resources":[{"name":"apiservers","singularName":"apiserver","namespaced":false,"kind":"APIServer","verbs":["get"]}]
			}`))
		default:
			w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newResolveProfilePolicyTestServer(t *testing.T, profile *configv1.TLSSecurityProfile, adherence configv1.TLSAdherencePolicy) *httptest.Server {
	t.Helper()
	apiServer := &configv1.APIServer{
		TypeMeta:   metav1.TypeMeta{APIVersion: "config.openshift.io/v1", Kind: "APIServer"},
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: profile,
			TLSAdherence:       adherence,
		},
	}
	body, err := json.Marshal(apiServer)
	if err != nil {
		t.Fatalf("failed to marshal APIServer test object: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/apiservers/cluster"):
			w.Write(body)
		case r.URL.Path == apiGroupListPath:
			w.Write([]byte(`{
				"kind":"APIGroupList",
				"apiVersion":"v1",
				"groups":[{
					"name":"config.openshift.io",
					"versions":[{"groupVersion":"config.openshift.io/v1","version":"v1"}],
					"preferredVersion":{"groupVersion":"config.openshift.io/v1","version":"v1"}
				}]
			}`))
		case r.URL.Path == configAPIResourceListPath:
			w.Write([]byte(`{
				"kind":"APIResourceList",
				"groupVersion":"config.openshift.io/v1",
				"resources":[{"name":"apiservers","singularName":"apiserver","namespaced":false,"kind":"APIServer","verbs":["get"]}]
			}`))
		default:
			w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolve_EmptyCustomCiphersDependsOnAdherence(t *testing.T) {
	tests := []struct {
		name      string
		adherence configv1.TLSAdherencePolicy
		wantError bool
	}{
		{name: "strict", adherence: configv1.TLSAdherencePolicyStrictAllComponents, wantError: true},
		{name: "unknown", adherence: "FuturePolicy", wantError: true},
		{name: "no opinion", adherence: configv1.TLSAdherencePolicyNoOpinion},
		{name: "legacy", adherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileCustomType,
				Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
					MinTLSVersion: configv1.VersionTLS12,
				}},
			}
			srv := newResolveProfilePolicyTestServer(t, profile, tt.adherence)
			result, err := Resolve(&restclient.Config{Host: srv.URL}, logr.Discard())
			if tt.wantError {
				if err == nil {
					t.Fatal("expected empty sub-TLS-1.3 cipher list to fail under strict adherence")
				}
				if !strings.Contains(err.Error(), "cipher") {
					t.Errorf("expected cipher validation error, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected empty cipher list to remain unrestricted, got error: %v", err)
			}
			got := applyTLSOpts(t, result.TLSOpts)
			if got.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion = %x, want TLS 1.2", got.MinVersion)
			}
			if len(got.CipherSuites) != 0 {
				t.Errorf("CipherSuites = %v, want no restriction", got.CipherSuites)
			}
		})
	}
}

func assertIntermediateProfileTLSOpts(t *testing.T, result *Result, profileRead bool) {
	t.Helper()
	profile := *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	if result.Profile.MinTLSVersion != profile.MinTLSVersion {
		t.Errorf("profile minimum = %q, want %q", result.Profile.MinTLSVersion, profile.MinTLSVersion)
	}

	var want *tls.Config
	if profileRead {
		wantOpts, err := buildProfileTLSOpts(profile, logr.Discard())
		if err != nil {
			t.Fatalf("failed to build expected Intermediate TLS options: %v", err)
		}
		want = applyTLSOpts(t, wantOpts)
	} else {
		wantFn, _ := tlspkg.NewTLSConfigFromProfile(profile)
		want = &tls.Config{}
		wantFn(want)
	}
	got := applyTLSOpts(t, result.TLSOpts)
	if got.MinVersion != want.MinVersion {
		t.Errorf("MinVersion = %x, want profile-derived %x", got.MinVersion, want.MinVersion)
	}
	if !slices.Equal(got.CipherSuites, want.CipherSuites) {
		t.Errorf("CipherSuites = %v, want profile-derived %v", got.CipherSuites, want.CipherSuites)
	}
	if !slices.Equal(got.NextProtos, []string{"h2", "http/1.1"}) {
		t.Errorf("NextProtos = %v, want [h2 http/1.1]", got.NextProtos)
	}
}

// TestResolve_AdherenceDeniedFailsStartup verifies a denied adherence read
// fails TLS setup after the valid profile read succeeds.
func TestResolve_AdherenceDeniedFailsStartup(t *testing.T) {
	srv := newResolveTestServer(t, http.StatusForbidden)

	_, err := Resolve(&restclient.Config{Host: srv.URL}, logr.Discard())
	if err == nil {
		t.Fatal("expected Resolve to fail when the adherence read is denied")
	}
	if !strings.Contains(err.Error(), "adherence") {
		t.Errorf("expected an adherence read error, got: %v", err)
	}
}

func newResolveProfileErrorTestServer(t *testing.T, apiAvailable bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == apiGroupListPath:
			if apiAvailable {
				w.Write([]byte(`{
					"kind":"APIGroupList",
					"apiVersion":"v1",
					"groups":[{
						"name":"config.openshift.io",
						"versions":[{"groupVersion":"config.openshift.io/v1","version":"v1"}],
						"preferredVersion":{"groupVersion":"config.openshift.io/v1","version":"v1"}
					}]
				}`))
			} else {
				w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`))
			}
		case r.URL.Path == configAPIResourceListPath:
			w.Write([]byte(`{
				"kind":"APIResourceList",
				"groupVersion":"config.openshift.io/v1",
				"resources":[{"name":"apiservers","singularName":"apiserver","namespaced":false,"kind":"APIServer","verbs":["get"]}]
			}`))
		case strings.HasSuffix(r.URL.Path, "/apiservers/cluster"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"APIServer not found","reason":"NotFound","code":404}`))
		default:
			w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolve_NonTransientProfileReadFallbackDoesNotRegisterWatcher(t *testing.T) {
	for _, tc := range []struct {
		name         string
		apiAvailable bool
	}{
		{name: "NoMatch"},
		{name: "NotFound", apiAvailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newResolveProfileErrorTestServer(t, tc.apiAvailable)
			result, err := Resolve(&restclient.Config{Host: srv.URL}, logr.Discard())
			if err != nil {
				t.Fatalf("expected profile fallback, got error: %v", err)
			}
			if result.HasOpenShiftConfigAPI {
				t.Error("HasOpenShiftConfigAPI = true, want false for NoMatch and NotFound")
			}
			assertIntermediateProfileTLSOpts(t, result, false)
		})
	}
}

func TestResolve_AdherenceTransientReadUsesNoOpinion(t *testing.T) {
	srv := newResolveTestServer(t, http.StatusServiceUnavailable)

	result, err := Resolve(&restclient.Config{Host: srv.URL}, logr.Discard())
	if err != nil {
		t.Fatalf("expected transient adherence read to fall back, got error: %v", err)
	}
	if result.AdherencePolicy != configv1.TLSAdherencePolicyNoOpinion {
		t.Errorf("adherence policy = %q, want NoOpinion", result.AdherencePolicy)
	}
	if !result.HasOpenShiftConfigAPI {
		t.Error("HasOpenShiftConfigAPI = false, want true after a successful profile read")
	}
	assertIntermediateProfileTLSOpts(t, result, true)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestResolve_ProfileTransportTimeoutUsesIntermediateFallback(t *testing.T) {
	srv := newResolveTestServer(t, http.StatusServiceUnavailable)
	transport := srv.Client().Transport
	cfg := &restclient.Config{
		Host: srv.URL,
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/apiservers/cluster") {
				return nil, &url.Error{
					Op: "Get", URL: r.URL.String(),
					Err: &net.DNSError{IsTimeout: true, Err: "i/o timeout", Name: "api.example.test"},
				}
			}
			return transport.RoundTrip(r)
		}),
	}

	result, err := Resolve(cfg, logr.Discard())
	if err != nil {
		t.Fatalf("expected transport timeout to use Intermediate fallback, got error: %v", err)
	}
	if !result.HasOpenShiftConfigAPI {
		t.Error("HasOpenShiftConfigAPI = false, want true after a transient profile-read error")
	}
	if result.AdherencePolicy != configv1.TLSAdherencePolicyNoOpinion {
		t.Errorf("adherence policy = %q, want NoOpinion", result.AdherencePolicy)
	}
	assertIntermediateProfileTLSOpts(t, result, false)
}

func TestBuildProfileTLSOpts_InvalidMinimumVersion(t *testing.T) {
	profile := configv1.TLSProfileSpec{
		MinTLSVersion: "VersionTLS09",
		Ciphers:       []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
	}

	// Must return an error instead of panicking inside TLSVersionOrDie.
	if _, err := buildProfileTLSOpts(profile, logr.Discard()); err == nil {
		t.Error("expected an error for an invalid minimum TLS version")
	}
}

func TestBuildProfileTLSOpts_UnusableCipherSuites(t *testing.T) {
	// All ciphers map to TLS 1.3-only IDs, unusable for a TLS 1.2 minimum.
	profile := configv1.TLSProfileSpec{
		MinTLSVersion: configv1.VersionTLS12,
		Ciphers:       []string{"TLS_AES_128_GCM_SHA256", "TLS_CHACHA20_POLY1305_SHA256"},
	}

	_, err := buildProfileTLSOpts(profile, logr.Discard())
	if err == nil {
		t.Fatal("expected an error when no configured cipher is usable for TLS <= 1.2")
	}
	if !strings.Contains(err.Error(), "cipher") {
		t.Errorf("expected a cipher validation error, got: %v", err)
	}
}

func TestBuildProfileTLSOpts_MixedCipherSuitesKeepUsableSubset(t *testing.T) {
	profile := configv1.TLSProfileSpec{
		MinTLSVersion: configv1.VersionTLS12,
		Ciphers: []string{
			"TLS_AES_128_GCM_SHA256",                // TLS 1.3-only, not usable for TLS <= 1.2
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", // usable
		},
	}

	opts, err := buildProfileTLSOpts(profile, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg := applyTLSOpts(t, opts)
	want := []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}
	if !slices.Equal(cfg.CipherSuites, want) {
		t.Errorf("CipherSuites = %v, want the usable subset %v", cfg.CipherSuites, want)
	}
}

func TestBuildProfileTLSOpts_ModernKeepsGoManagedTLS13Suites(t *testing.T) {
	opts, err := buildProfileTLSOpts(*configv1.TLSProfiles[configv1.TLSProfileModernType], logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg := applyTLSOpts(t, opts)
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.CipherSuites != nil {
		t.Errorf("CipherSuites = %v, want nil so Go manages the TLS 1.3 suites", cfg.CipherSuites)
	}
}

func TestBuildProfileTLSOpts_OldProfileMinimumHonored(t *testing.T) {
	opts, err := buildProfileTLSOpts(*configv1.TLSProfiles[configv1.TLSProfileOldType], logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg := applyTLSOpts(t, opts)
	if cfg.MinVersion != tls.VersionTLS10 {
		t.Errorf("MinVersion = %x, want the Old profile minimum TLS 1.0 honored exactly", cfg.MinVersion)
	}
	if len(cfg.CipherSuites) == 0 {
		t.Error("expected the Old profile to retain usable cipher suites")
	}
}
