package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type tlsReader struct {
	client.Reader
	get func(context.Context, client.ObjectKey, client.Object) error
}

func (r tlsReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	return r.get(ctx, key, obj)
}

func TestReadTLSConfig(t *testing.T) {
	for _, policy := range []configv1.TLSAdherencePolicy{"", configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly, configv1.TLSAdherencePolicyStrictAllComponents, "future-policy"} {
		t.Run(string(policy), func(t *testing.T) {
			apiServer := &configv1.APIServer{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Spec: configv1.APIServerSpec{
					TLSAdherence:       policy,
					TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileOldType},
				},
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(apiServer).Build()
			result, err := readTLSConfig(context.Background(), reader)
			if err != nil {
				t.Fatal(err)
			}
			if !result.hasOpenShiftConfigAPI || result.adherencePolicy != policy {
				t.Fatalf("lost API/adherence information: %+v", result)
			}
			cfg := applyTLSOptions(result.tlsOpts)
			if cfg.MinVersion != tls.VersionTLS10 {
				t.Fatalf("Old minimum changed: got %x", cfg.MinVersion)
			}
		})
	}
}

func TestTLSReadFailures(t *testing.T) {
	resource := schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}
	for _, tc := range []struct {
		name     string
		err      error
		fallback bool
	}{
		{"forbidden", apierrors.NewForbidden(resource, "cluster", errors.New("denied")), false},
		{"unauthorized", apierrors.NewUnauthorized("denied"), false},
		{"missing API", &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "config.openshift.io", Kind: "APIServer"}}, true},
		{"missing resource", apierrors.NewNotFound(resource, "cluster"), true},
		{"unavailable", apierrors.NewServiceUnavailable("temporary"), true},
		{"internal server error", apierrors.NewInternalError(errors.New("temporary")), true},
		{"timeout", apierrors.NewTimeoutError("temporary", 0), true},
		{"server timeout", apierrors.NewServerTimeout(resource, "get", 0), true},
		{"throttled", apierrors.NewTooManyRequests("temporary", 0), true},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"unexpected", errors.New("invalid response"), false},
		{"denied discovery", &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
			{Group: "config.openshift.io", Version: "v1"}: apierrors.NewUnauthorized("denied"),
			{Group: "other", Version: "v1"}:               apierrors.NewServiceUnavailable("temporary"),
		}}, false},
		{"transient discovery", &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
			{Group: "config.openshift.io", Version: "v1"}: apierrors.NewServiceUnavailable("temporary"),
		}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := tlsReader{get: func(_ context.Context, key client.ObjectKey, _ client.Object) error {
				if key.Name != "cluster" || key.Namespace != "" {
					t.Fatalf("unexpected object key: %v", key)
				}
				return fmt.Errorf("failed to read test configuration: %w", tc.err)
			}}
			result, err := readTLSConfig(context.Background(), reader)
			if !tc.fallback {
				if !errors.Is(err, tc.err) {
					t.Fatalf("expected original failure %v; got %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.profile, *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]) {
				t.Fatalf("fallback is not Intermediate: %+v", result.profile)
			}
			if cfg := applyTLSOptions(result.tlsOpts); cfg.MinVersion != tls.VersionTLS12 || len(cfg.CipherSuites) == 0 {
				t.Fatalf("fallback not applied: %+v", cfg)
			}
		})
	}
}

func TestTLSProfileValidation(t *testing.T) {
	strict := configv1.TLSAdherencePolicyStrictAllComponents
	for _, tc := range []struct {
		name       string
		minimum    configv1.TLSProtocolVersion
		ciphers    []string
		policy     configv1.TLSAdherencePolicy
		wantErr    bool
		wantSuites []uint16
	}{
		{"invalid minimum", "TLS-invalid", nil, strict, true, nil},
		{"empty minimum", "", nil, strict, true, nil},
		{"unknown cipher", configv1.VersionTLS12, []string{"not-a-cipher"}, strict, true, nil},
		{"unimplemented cipher", configv1.VersionTLS12, []string{"TLS_RSA_WITH_AES_256_CBC_SHA256"}, strict, true, nil},
		{"TLS13-only request below TLS13", configv1.VersionTLS12, []string{"TLS_AES_128_GCM_SHA256"}, strict, true, nil},
		{"empty strict", configv1.VersionTLS12, nil, strict, true, nil},
		{"empty unknown policy", configv1.VersionTLS12, nil, "future-policy", true, nil},
		{"empty no opinion", configv1.VersionTLS12, nil, "", false, nil},
		{"empty legacy", configv1.VersionTLS12, nil, configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly, false, nil},
		{"mixed", configv1.VersionTLS12, []string{"not-a-cipher", "TLS_AES_128_GCM_SHA256", "TLS_RSA_WITH_AES_256_CBC_SHA256", "ECDHE-RSA-AES128-GCM-SHA256"}, strict, false, []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}},
		{"IANA name", configv1.VersionTLS12, []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"}, strict, false, []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}},
		{"Modern", configv1.VersionTLS13, configv1.TLSProfiles[configv1.TLSProfileModernType].Ciphers, strict, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := tlsOptionsFromProfile(configv1.TLSProfileSpec{MinTLSVersion: tc.minimum, Ciphers: tc.ciphers}, tc.policy)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected profile to be rejected")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg := applyTLSOptions(opts)
			if !reflect.DeepEqual(cfg.CipherSuites, tc.wantSuites) {
				t.Fatalf("suites = %v, want %v", cfg.CipherSuites, tc.wantSuites)
			}
		})
	}
}

func TestStrictMinimumHonored(t *testing.T) {
	for _, policy := range []configv1.TLSAdherencePolicy{configv1.TLSAdherencePolicyStrictAllComponents, "future-policy"} {
		for _, tc := range []struct {
			name    configv1.TLSProtocolVersion
			version uint16
		}{
			{configv1.VersionTLS10, tls.VersionTLS10},
			{configv1.VersionTLS11, tls.VersionTLS11},
			{configv1.VersionTLS12, tls.VersionTLS12},
			{configv1.VersionTLS13, tls.VersionTLS13},
		} {
			t.Run(string(policy)+"/"+string(tc.name), func(t *testing.T) {
				opts, err := tlsOptionsFromProfile(configv1.TLSProfileSpec{
					MinTLSVersion: tc.name, Ciphers: []string{"ECDHE-RSA-AES128-SHA"},
				}, policy)
				if err != nil {
					t.Fatal(err)
				}
				if cfg := applyTLSOptions(opts); cfg.MinVersion != tc.version {
					t.Fatalf("minimum = %x, want %x", cfg.MinVersion, tc.version)
				}
			})
		}
	}
}

func TestMalformedCustomProfile(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec:       configv1.APIServerSpec{TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType}},
	}).Build()
	if _, err := readTLSConfig(context.Background(), reader); err == nil {
		t.Fatal("invalid custom profile silently accepted")
	}
}

func TestTLSWatcher(t *testing.T) {
	for _, change := range []string{"unchanged", "profile", "adherence"} {
		t.Run(change, func(t *testing.T) {
			apiServer := &configv1.APIServer{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Spec: configv1.APIServerSpec{
					TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
					TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileIntermediateType},
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(apiServer).Build()
			result, err := readTLSConfig(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			watcher := result.securityProfileWatcher(c, cancel)
			switch change {
			case "profile":
				apiServer.Spec.TLSSecurityProfile.Type = configv1.TLSProfileModernType
			case "adherence":
				apiServer.Spec.TLSAdherence = configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly
			}
			if err := c.Update(context.Background(), apiServer); err != nil {
				t.Fatal(err)
			}
			if _, err := watcher.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "cluster"}}); err != nil {
				t.Fatal(err)
			}
			if got := ctx.Err() != nil; got != (change != "unchanged") {
				t.Fatalf("graceful shutdown = %v for %s", got, change)
			}
		})
	}
}

func TestTLSFallbackRecovery(t *testing.T) {
	result, err := readTLSConfig(context.Background(), tlsReader{get: func(context.Context, client.ObjectKey, client.Object) error {
		return apierrors.NewServiceUnavailable("temporary")
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.retryTLSConfig {
		t.Fatal("transient fallback must keep checking cluster policy")
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: configv1.APIServerSpec{
			TLSAdherence:       configv1.TLSAdherencePolicyStrictAllComponents,
			TLSSecurityProfile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType},
		},
	}).Build()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w := &tlsFallbackWatcher{watcher: result.securityProfileWatcher(c, cancel)}
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("recovered policy did not trigger reload")
	}
}

type tlsRoundTripper func(*http.Request) (*http.Response, error)

func (f tlsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTLSBootstrapTimeout(t *testing.T) {
	// Block discovery, which does not receive the outer Get context. The
	// bootstrap HTTP client's timeout must still interrupt it.
	var requests atomic.Int32
	var bounded atomic.Bool
	cfg := &rest.Config{
		Host: "https://tls-test.invalid",
		Transport: tlsRoundTripper(func(r *http.Request) (*http.Response, error) {
			requests.Add(1)
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > tlsBootstrapTimeout+time.Second {
				return nil, errors.New("discovery request has no bounded deadline")
			}
			bounded.Store(true)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}),
	}
	start := time.Now()
	result, err := setupTLS(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() == 0 || !bounded.Load() || time.Since(start) > 2*tlsBootstrapTimeout+3*time.Second {
		t.Fatalf("discovery was not bounded: requests=%d elapsed=%s", requests.Load(), time.Since(start))
	}
	if cfg.Timeout != 0 {
		t.Fatal("manager REST config timeout was mutated")
	}
	if result.profile.MinTLSVersion != configv1.VersionTLS12 {
		t.Fatal("did not fall back to Intermediate")
	}
}

func TestTLSBootstrapDenied(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			cfg := &rest.Config{
				Host: "https://tls-test.invalid",
				Transport: tlsRoundTripper(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
						Body:    io.NopCloser(strings.NewReader(fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":%q,"code":%d}`, http.StatusText(code), code))),
						Request: r,
					}, nil
				}),
			}
			if _, err := setupTLS(context.Background(), cfg); err == nil {
				t.Fatal("denied discovery read allowed startup")
			}
		})
	}
}

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
			opts, err := tlsOptionsFromProfile(configv1.TLSProfileSpec{
				MinTLSVersion: tc.minimum, Ciphers: []string{"ECDHE-ECDSA-AES128-SHA"},
			}, configv1.TLSAdherencePolicyStrictAllComponents)
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
