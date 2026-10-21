package tls

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const tlsBootstrapTimeout = 10 * time.Second

// Result holds the metrics TLS configuration resolved from the cluster policy.
type Result struct {
	TLSOpts               []func(*tls.Config)
	Profile               configv1.TLSProfileSpec
	AdherencePolicy       configv1.TLSAdherencePolicy
	HasOpenShiftConfigAPI bool
	retryTLSConfig        bool
	bootstrapClient       client.Client
}

// Resolve reads the cluster TLS profile and adherence policy with bounded startup requests.
func Resolve(ctx context.Context, cfg *rest.Config, logger logr.Logger) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, tlsBootstrapTimeout)
	defer cancel()

	// REST mapper discovery does not use the Get context, so bound the HTTP
	// client as well. Do not change timeouts on the manager's REST config.
	bootstrapConfig := rest.CopyConfig(cfg)
	bootstrapConfig.Timeout = tlsBootstrapTimeout
	scheme := runtime.NewScheme()
	if err := configv1.Install(scheme); err != nil {
		return Result{}, fmt.Errorf("failed to register OpenShift config API scheme: %w", err)
	}
	bootstrapClient, err := client.New(bootstrapConfig, client.Options{Scheme: scheme})
	if err != nil {
		return Result{}, fmt.Errorf("failed to create TLS bootstrap client: %w", err)
	}
	result, err := readTLSConfig(ctx, bootstrapClient, logger)
	result.bootstrapClient = bootstrapClient
	return result, err
}

func readTLSConfig(ctx context.Context, reader client.Reader, logger logr.Logger) (Result, error) {
	var result Result
	apiServer := &configv1.APIServer{}
	// Both settings live on the same object. Read them atomically so a denied
	// read cannot be confused with an unset adherence policy.
	err := reader.Get(ctx, client.ObjectKey{Name: tlspkg.APIServerName}, apiServer)
	if err != nil {
		if !canFallbackTLS(err) {
			return result, fmt.Errorf("failed to read APIServer TLS configuration: %w", err)
		}
		logger.Info("Unable to read cluster TLS configuration, using Intermediate TLS profile", "error", err)
		result.Profile = *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
		// A missing object can be created later; keep watching when discovery
		// established that the API exists.
		result.HasOpenShiftConfigAPI = apierrors.IsNotFound(err)
		result.retryTLSConfig = !result.HasOpenShiftConfigAPI && !apimeta.IsNoMatchError(err)
	} else {
		result.HasOpenShiftConfigAPI = true
		result.AdherencePolicy = apiServer.Spec.TLSAdherence
		result.Profile, err = tlspkg.GetTLSProfileSpec(apiServer.Spec.TLSSecurityProfile)
		if err != nil {
			return result, fmt.Errorf("failed to resolve APIServer TLS profile: %w", err)
		}
	}

	result.TLSOpts, err = OptionsFromProfile(result.Profile, result.AdherencePolicy, logger)
	return result, err
}

func canFallbackTLS(err error) bool {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return false
	}
	// Discovery may wrap per-group errors. Never hide a denied discovery read
	// behind a fallback, including mixed transient and permission failures.
	var groupErrors *discovery.ErrGroupDiscoveryFailed
	if errors.As(err, &groupErrors) {
		if len(groupErrors.Groups) == 0 {
			return false
		}
		for _, groupErr := range groupErrors.Groups {
			if !canFallbackTLS(groupErr) {
				return false
			}
		}
		return true
	}
	var networkError net.Error
	return apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError)
}

// OptionsFromProfile validates a resolved profile and returns server TLS options.
func OptionsFromProfile(profile configv1.TLSProfileSpec, adherence configv1.TLSAdherencePolicy, logger logr.Logger) ([]func(*tls.Config), error) {
	// Validate before calling the helper, which otherwise panics on bad input.
	switch profile.MinTLSVersion {
	case configv1.VersionTLS10, configv1.VersionTLS11, configv1.VersionTLS12, configv1.VersionTLS13:
	default:
		return nil, fmt.Errorf("failed to configure TLS: invalid minimum TLS version %q", profile.MinTLSVersion)
	}

	configure, unsupported := tlspkg.NewTLSConfigFromProfile(profile)
	resolved := &tls.Config{}
	configure(resolved)
	strict := adherence != configv1.TLSAdherencePolicyNoOpinion &&
		adherence != configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly
	if strict && adherence != configv1.TLSAdherencePolicyStrictAllComponents {
		logger.Info("Unknown TLS adherence policy, treating as StrictAllComponents", "policy", adherence)
	}

	if resolved.MinVersion < tls.VersionTLS13 {
		// The helper also maps TLS 1.3 IDs and Go constants that have no
		// runtime implementation. Neither is usable in Config.CipherSuites.
		runtimeSuites := make(map[uint16]bool)
		for _, suite := range append(tls.CipherSuites(), tls.InsecureCipherSuites()...) {
			for _, version := range suite.SupportedVersions {
				if version <= tls.VersionTLS12 {
					runtimeSuites[suite.ID] = true
				}
			}
		}
		var usable []uint16
		for _, id := range resolved.CipherSuites {
			if runtimeSuites[id] {
				usable = append(usable, id)
			}
		}
		if len(usable) == 0 && (len(profile.Ciphers) > 0 || strict) {
			return nil, fmt.Errorf("failed to configure TLS: profile has no usable TLS <= 1.2 cipher suites")
		}
		resolved.CipherSuites = usable
	}
	if len(unsupported) > 0 {
		logger.Info("Ignoring unsupported TLS cipher names", "ciphers", unsupported)
	}
	return []func(*tls.Config){func(c *tls.Config) {
		c.MinVersion = resolved.MinVersion
		c.CipherSuites = resolved.CipherSuites
		c.NextProtos = []string{"h2", "http/1.1"}
	}}, nil
}

// SetupWatcher registers policy-change shutdown callbacks and recovery after a transient fallback.
func SetupWatcher(mgr manager.Manager, result Result, cancel context.CancelFunc, logger logr.Logger) error {
	if result.HasOpenShiftConfigAPI {
		if err := result.securityProfileWatcher(mgr.GetClient(), cancel, logger).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("failed to register TLS security profile watcher: %w", err)
		}
	} else if result.retryTLSConfig {
		if err := mgr.Add(&tlsFallbackWatcher{watcher: result.securityProfileWatcher(result.bootstrapClient, cancel, logger)}); err != nil {
			return fmt.Errorf("failed to register TLS fallback watcher: %w", err)
		}
	}
	return nil
}

func (r Result) securityProfileWatcher(reader client.Client, cancel context.CancelFunc, logger logr.Logger) *tlspkg.SecurityProfileWatcher {
	return &tlspkg.SecurityProfileWatcher{
		Client:                    reader,
		InitialTLSProfileSpec:     r.Profile,
		InitialTLSAdherencePolicy: r.AdherencePolicy,
		OnProfileChange: func(_ context.Context, _, _ configv1.TLSProfileSpec) {
			logger.Info("TLS profile changed, initiating graceful shutdown to reload")
			cancel()
		},
		OnAdherencePolicyChange: func(_ context.Context, _, _ configv1.TLSAdherencePolicy) {
			logger.Info("TLS adherence policy changed, initiating graceful shutdown to reload")
			cancel()
		},
	}
}

// When discovery or the initial read fails transiently, registering an informer
// can fail for the same reason. Retry through the bounded bootstrap client so
// the fallback does not permanently opt out of cluster policy changes.
type tlsFallbackWatcher struct {
	watcher *tlspkg.SecurityProfileWatcher
}

func (*tlsFallbackWatcher) NeedLeaderElection() bool { return false }

func (w *tlsFallbackWatcher) Start(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, tlsBootstrapTimeout)
		_, err := w.watcher.Reconcile(readCtx, ctrl.Request{NamespacedName: client.ObjectKey{Name: tlspkg.APIServerName}})
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !canFallbackTLS(err) {
			return fmt.Errorf("failed to watch TLS configuration after startup fallback: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
