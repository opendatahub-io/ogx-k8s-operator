package ogx

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
	moduleconfig "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/config"
	common "github.com/opendatahub-io/odh-platform-utilities/api/common"
	odhgc "github.com/opendatahub-io/odh-platform-utilities/pkg/controller/gc"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestManagementState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state common.ManagementState
		want  common.ManagementState
	}{
		{name: "managed", state: common.Managed, want: common.Managed},
		{name: "removed", state: common.Removed, want: common.Removed},
		{name: "empty defaults to managed", state: "", want: common.Managed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			instance := &platformv1alpha1.OGX{
				Spec: platformv1alpha1.OGXSpec{
					ManagementSpec: common.ManagementSpec{ManagementState: tt.state},
				},
			}

			if got := managementState(instance); got != tt.want {
				t.Fatalf("managementState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFinalizerHelpers(t *testing.T) {
	t.Parallel()

	instance := &platformv1alpha1.OGX{}

	if !ensureFinalizer(instance) {
		t.Fatal("expected ensureFinalizer to add finalizer")
	}
	if !containsFinalizer(instance, ogxFinalizer) {
		t.Fatal("expected finalizer to be present")
	}
	if ensureFinalizer(instance) {
		t.Fatal("expected ensureFinalizer to be idempotent")
	}
	if !clearFinalizer(instance) {
		t.Fatal("expected clearFinalizer to remove finalizer")
	}
	if containsFinalizer(instance, ogxFinalizer) {
		t.Fatal("expected finalizer to be removed")
	}
}

func TestAggregateOGXServerHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		objects       []runtime.Object
		wantTotal     int
		wantUnhealthy int
	}{
		{
			name:          "empty cluster",
			wantTotal:     0,
			wantUnhealthy: 0,
		},
		{
			name:          "healthy ogxserver",
			objects:       []runtime.Object{newOGXServerUnstructured("healthy", "ns", condition(ogxServerDeploymentReady, metav1.ConditionTrue), condition(ogxServerHealthCheck, metav1.ConditionTrue))},
			wantTotal:     1,
			wantUnhealthy: 0,
		},
		{
			name:          "deployment not ready is unhealthy",
			objects:       []runtime.Object{newOGXServerUnstructured("unhealthy", "ns", condition(ogxServerDeploymentReady, metav1.ConditionFalse))},
			wantTotal:     1,
			wantUnhealthy: 1,
		},
		{
			name:          "no conditions is unhealthy",
			objects:       []runtime.Object{newOGXServerUnstructuredNoConditions("no-conditions", "ns")},
			wantTotal:     1,
			wantUnhealthy: 1,
		},
		{
			name:          "mixed health",
			objects:       []runtime.Object{newOGXServerUnstructured("healthy", "ns-a", condition(ogxServerDeploymentReady, metav1.ConditionTrue), condition(ogxServerHealthCheck, metav1.ConditionTrue)), newOGXServerUnstructured("unhealthy", "ns-b", condition(ogxServerHealthCheck, metav1.ConditionFalse))},
			wantTotal:     2,
			wantUnhealthy: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli := fake.NewClientBuilder().WithRuntimeObjects(tt.objects...).Build()
			reconciler := &Reconciler{
				Client:    cli,
				APIReader: cli,
			}

			got, err := reconciler.aggregateOGXServerHealth(context.Background())
			if err != nil {
				t.Fatalf("aggregateOGXServerHealth() error = %v", err)
			}
			if got.Total != tt.wantTotal {
				t.Fatalf("aggregateOGXServerHealth().Total = %d, want %d", got.Total, tt.wantTotal)
			}
			if got.UnhealthyCount != tt.wantUnhealthy {
				t.Fatalf("aggregateOGXServerHealth().UnhealthyCount = %d, want %d", got.UnhealthyCount, tt.wantUnhealthy)
			}
		})
	}
}

func TestRootWebhookResourcesReady(t *testing.T) {
	t.Parallel()

	renderedWebhook := newRenderedValidatingWebhook("validating-webhook-configuration", "webhook-service", "ns-a")
	renderedDeployment := newRenderedWebhookDeployment("root-operator", "ns-a", "ogx-k8s-operator-webhook-cert")
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-service",
			Namespace: "ns-a",
		},
	}
	actualWebhook := &admissionv1.ValidatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionv1.SchemeGroupVersion.String(),
			Kind:       "ValidatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "validating-webhook-configuration",
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name: "vogxserver.kb.io",
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      "webhook-service",
						Namespace: "ns-a",
					},
				},
			},
		},
	}
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ogx-k8s-operator-webhook-cert",
			Namespace: "ns-a",
		},
	}

	tests := []struct {
		name     string
		objects  []runtime.Object
		rendered []unstructured.Unstructured
		want     bool
		wantErr  bool
	}{
		{
			name:     "no webhooks rendered",
			rendered: nil,
			want:     true,
		},
		{
			name:     "webhook service and secret ready",
			objects:  []runtime.Object{actualWebhook, service, secret},
			rendered: []unstructured.Unstructured{renderedWebhook, renderedDeployment},
			want:     true,
		},
		{
			name:     "webhook missing",
			rendered: []unstructured.Unstructured{renderedWebhook, renderedDeployment},
			want:     false,
		},
		{
			name:     "service missing",
			objects:  []runtime.Object{actualWebhook, secret},
			rendered: []unstructured.Unstructured{renderedWebhook, renderedDeployment},
			want:     false,
		},
		{
			name:     "secret missing",
			objects:  []runtime.Object{actualWebhook, service},
			rendered: []unstructured.Unstructured{renderedWebhook, renderedDeployment},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cli := fake.NewClientBuilder().WithRuntimeObjects(tt.objects...).Build()
			reconciler := &Reconciler{
				Client:    cli,
				APIReader: cli,
			}

			got, err := reconciler.rootWebhookResourcesReady(context.Background(), tt.rendered)
			if (err != nil) != tt.wantErr {
				t.Fatalf("rootWebhookResourcesReady() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("rootWebhookResourcesReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

type fakeGCRunner struct {
	called bool
	err    error
}

func (f *fakeGCRunner) Run(_ context.Context, _ odhgc.RunParams) error {
	f.called = true
	return f.err
}

func TestReconcileManagedAddsFinalizer(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(clientgoscheme): %v", err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(platformv1alpha1): %v", err)
	}

	manifestsRoot := writeMinimalManifestTree(t)
	instance := &platformv1alpha1.OGX{
		TypeMeta: metav1.TypeMeta{
			APIVersion: platformv1alpha1.GroupVersion.String(),
			Kind:       platformv1alpha1.OGXKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       platformv1alpha1.OGXInstanceName,
			Generation: 3,
		},
		Spec: platformv1alpha1.OGXSpec{
			ManagementSpec: common.ManagementSpec{ManagementState: common.Managed},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.OGX{}).
		WithObjects(instance).
		Build()

	reconciler := NewReconciler(cli, cli, scheme, &moduleconfig.Config{
		ManifestsPath:         manifestsRoot,
		ApplicationsNamespace: "opendatahub-ogx-system",
		PlatformName:          "OpenDataHub",
		PlatformVersion:       "test-version",
	}, nil, nil)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	updated := &platformv1alpha1.OGX{}
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(instance), updated); err != nil {
		t.Fatalf("Get(updated OGX): %v", err)
	}
	if !containsFinalizer(updated, ogxFinalizer) {
		t.Fatal("expected reconcile to add finalizer")
	}
}

func TestReconcileRemovedClearsFinalizerAndRunsGC(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(clientgoscheme): %v", err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(platformv1alpha1): %v", err)
	}

	manifestsRoot := writeMinimalManifestTree(t)
	instance := &platformv1alpha1.OGX{
		TypeMeta: metav1.TypeMeta{
			APIVersion: platformv1alpha1.GroupVersion.String(),
			Kind:       platformv1alpha1.OGXKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       platformv1alpha1.OGXInstanceName,
			Generation: 4,
			Finalizers: []string{ogxFinalizer},
		},
		Spec: platformv1alpha1.OGXSpec{
			ManagementSpec: common.ManagementSpec{ManagementState: common.Removed},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.OGX{}).
		WithObjects(instance).
		Build()

	gcRunner := &fakeGCRunner{}
	reconciler := NewReconciler(cli, cli, scheme, &moduleconfig.Config{
		ManifestsPath:         manifestsRoot,
		ApplicationsNamespace: "opendatahub-ogx-system",
		PlatformName:          "OpenDataHub",
		PlatformVersion:       "test-version",
	}, nil, nil)
	reconciler.GarbageCollector = gcRunner

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	updated := &platformv1alpha1.OGX{}
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(instance), updated); err != nil {
		t.Fatalf("Get(updated OGX): %v", err)
	}
	if containsFinalizer(updated, ogxFinalizer) {
		t.Fatal("expected reconcile to remove finalizer")
	}
	if !gcRunner.called {
		t.Fatal("expected reconcile to invoke garbage collector")
	}
}

func TestReconcileReportsPlatformReleaseFromConfigMap(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(clientgoscheme): %v", err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(platformv1alpha1): %v", err)
	}

	const applicationsNS = "opendatahub"
	manifestsRoot := writeMinimalManifestTree(t)
	instance := &platformv1alpha1.OGX{
		TypeMeta: metav1.TypeMeta{
			APIVersion: platformv1alpha1.GroupVersion.String(),
			Kind:       platformv1alpha1.OGXKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       platformv1alpha1.OGXInstanceName,
			Generation: 1,
		},
		Spec: platformv1alpha1.OGXSpec{
			ManagementSpec: common.ManagementSpec{ManagementState: common.Managed},
		},
	}
	platformCM := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformConfigCMName,
			Namespace: applicationsNS,
		},
		Data: map[string]string{
			"platform-name":        "OpenDataHub",
			platformVersionDataKey: "0.0.0",
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&platformv1alpha1.OGX{}).
		WithObjects(instance, platformCM).
		Build()

	reconciler := NewReconciler(cli, cli, scheme, &moduleconfig.Config{
		ManifestsPath:         manifestsRoot,
		ApplicationsNamespace: applicationsNS,
		PlatformName:          "OpenDataHub",
		PlatformVersion:       moduleconfig.DefaultPlatformVersion,
	}, nil, nil)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	updated := &platformv1alpha1.OGX{}
	if err := cli.Get(context.Background(), client.ObjectKeyFromObject(instance), updated); err != nil {
		t.Fatalf("Get(updated OGX): %v", err)
	}

	assertPlatformHandshake(t, updated, "0.0.0")
}

func TestIsPlatformConfigMap(t *testing.T) {
	t.Parallel()

	reconciler := &Reconciler{
		Config: &moduleconfig.Config{ApplicationsNamespace: "opendatahub"},
	}

	tests := []struct {
		name      string
		objName   string
		namespace string
		want      bool
	}{
		{name: "platform configmap", objName: platformConfigCMName, namespace: "opendatahub", want: true},
		{name: "wrong name", objName: "other-config", namespace: "opendatahub", want: false},
		{name: "wrong namespace", objName: platformConfigCMName, namespace: "other", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tt.objName, Namespace: tt.namespace}}
			if got := reconciler.isPlatformConfigMap(obj); got != tt.want {
				t.Fatalf("isPlatformConfigMap() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetPlatformRelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing []common.ComponentRelease
		version  string
		want     []common.ComponentRelease
	}{
		{
			name:    "appends platform entry",
			version: "0.0.0",
			want:    []common.ComponentRelease{{Name: platformReleaseName, Version: "0.0.0"}},
		},
		{
			name:     "updates existing platform entry",
			existing: []common.ComponentRelease{{Name: platformReleaseName, Version: "old"}, {Name: "OGX", Version: "v1.2.1"}},
			version:  "0.0.0",
			want:     []common.ComponentRelease{{Name: platformReleaseName, Version: "0.0.0"}, {Name: "OGX", Version: "v1.2.1"}},
		},
		{
			name:     "skips unknown default",
			existing: []common.ComponentRelease{{Name: "OGX", Version: "v1.2.1"}},
			version:  moduleconfig.DefaultPlatformVersion,
			want:     []common.ComponentRelease{{Name: "OGX", Version: "v1.2.1"}},
		},
		{
			name:     "skips empty version",
			existing: []common.ComponentRelease{{Name: "OGX", Version: "v1.2.1"}},
			version:  "",
			want:     []common.ComponentRelease{{Name: "OGX", Version: "v1.2.1"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			status := common.ComponentReleaseStatus{Releases: append([]common.ComponentRelease(nil), tt.existing...)}
			setPlatformRelease(&status, tt.version)
			if len(status.Releases) != len(tt.want) {
				t.Fatalf("len(Releases) = %d, want %d", len(status.Releases), len(tt.want))
			}
			for i := range tt.want {
				if status.Releases[i] != tt.want[i] {
					t.Fatalf("Releases[%d] = %+v, want %+v", i, status.Releases[i], tt.want[i])
				}
			}
		})
	}
}

func TestUpdateStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		state              common.ManagementState
		provisioning       bool
		rootReady          bool
		webhookReady       bool
		health             ogxServerHealthSummary
		reconcileErr       error
		wantPhase          common.Phase
		wantReady          metav1.ConditionStatus
		wantProvisioned    metav1.ConditionStatus
		wantDegraded       metav1.ConditionStatus
		wantWebhookReady   metav1.ConditionStatus
		wantReleaseEntries int
	}{
		{
			name:               "ready and healthy",
			state:              common.Managed,
			provisioning:       true,
			rootReady:          true,
			webhookReady:       true,
			wantPhase:          common.PhaseReady,
			wantReady:          metav1.ConditionTrue,
			wantProvisioned:    metav1.ConditionTrue,
			wantDegraded:       metav1.ConditionFalse,
			wantWebhookReady:   metav1.ConditionTrue,
			wantReleaseEntries: 3,
		},
		{
			name:         "ready but degraded",
			state:        common.Managed,
			provisioning: true,
			rootReady:    true,
			webhookReady: true,
			health: ogxServerHealthSummary{
				Total:          2,
				UnhealthyCount: 1,
				Unhealthy:      []string{"ns-a/unhealthy"},
			},
			wantPhase:          common.PhaseReady,
			wantReady:          metav1.ConditionTrue,
			wantProvisioned:    metav1.ConditionTrue,
			wantDegraded:       metav1.ConditionTrue,
			wantWebhookReady:   metav1.ConditionTrue,
			wantReleaseEntries: 3,
		},
		{
			name:               "removed state",
			state:              common.Removed,
			wantPhase:          common.PhaseNotReady,
			wantReady:          metav1.ConditionFalse,
			wantProvisioned:    metav1.ConditionFalse,
			wantDegraded:       metav1.ConditionFalse,
			wantWebhookReady:   metav1.ConditionFalse,
			wantReleaseEntries: 3,
		},
		{
			name:               "webhook not ready keeps module not ready",
			state:              common.Managed,
			provisioning:       true,
			rootReady:          true,
			webhookReady:       false,
			wantPhase:          common.PhaseNotReady,
			wantReady:          metav1.ConditionFalse,
			wantProvisioned:    metav1.ConditionTrue,
			wantDegraded:       metav1.ConditionFalse,
			wantWebhookReady:   metav1.ConditionFalse,
			wantReleaseEntries: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme(clientgoscheme): %v", err)
			}
			if err := platformv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme(platformv1alpha1): %v", err)
			}

			manifestsRoot := writeComponentMetadata(t)
			instance := &platformv1alpha1.OGX{
				TypeMeta: metav1.TypeMeta{
					APIVersion: platformv1alpha1.GroupVersion.String(),
					Kind:       platformv1alpha1.OGXKind,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       platformv1alpha1.OGXInstanceName,
					Generation: 7,
				},
				Spec: platformv1alpha1.OGXSpec{
					ManagementSpec: common.ManagementSpec{ManagementState: tt.state},
				},
			}

			cli := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&platformv1alpha1.OGX{}).
				WithObjects(instance).
				Build()

			reconciler := &Reconciler{
				Client: cli,
				Scheme: scheme,
				Config: &moduleconfig.Config{
					ManifestsPath:   manifestsRoot,
					PlatformName:    "OpenDataHub",
					PlatformVersion: "test-version",
				},
			}

			if err := reconciler.updateStatus(context.Background(), instance, tt.provisioning, tt.rootReady, tt.webhookReady, tt.health, tt.reconcileErr); err != nil {
				t.Fatalf("updateStatus() error = %v", err)
			}

			updated := &platformv1alpha1.OGX{}
			if err := cli.Get(context.Background(), client.ObjectKeyFromObject(instance), updated); err != nil {
				t.Fatalf("Get(updated OGX): %v", err)
			}

			if updated.Status.ObservedGeneration != updated.Generation {
				t.Fatalf("ObservedGeneration = %d, want %d", updated.Status.ObservedGeneration, updated.Generation)
			}
			if updated.Status.Phase != tt.wantPhase {
				t.Fatalf("Phase = %q, want %q", updated.Status.Phase, tt.wantPhase)
			}
			if len(updated.Status.Releases) != tt.wantReleaseEntries {
				t.Fatalf("len(Releases) = %d, want %d", len(updated.Status.Releases), tt.wantReleaseEntries)
			}
			assertPlatformHandshake(t, updated, "test-version")

			assertConditionStatus(t, updated, string(common.ConditionTypeReady), tt.wantReady)
			assertConditionStatus(t, updated, string(common.ConditionTypeProvisioningSucceeded), tt.wantProvisioned)
			assertConditionStatus(t, updated, string(common.ConditionTypeDegraded), tt.wantDegraded)
			assertConditionStatus(t, updated, conditionTypeRootWebhookReady, tt.wantWebhookReady)
		})
	}
}

func assertPlatformHandshake(t *testing.T, instance *platformv1alpha1.OGX, version string) {
	t.Helper()

	if got := platformReleaseVersion(instance.Status.Releases); got != version {
		t.Fatalf("status.releases[name=%q].version = %q, want %q", platformReleaseName, got, version)
	}
	if instance.Status.Distribution.Version != version {
		t.Fatalf("Distribution.Version = %q, want %q", instance.Status.Distribution.Version, version)
	}
}

func platformReleaseVersion(releases []common.ComponentRelease) string {
	for _, release := range releases {
		if release.Name == platformReleaseName {
			return release.Version
		}
	}
	return ""
}

func containsFinalizer(instance *platformv1alpha1.OGX, value string) bool {
	for _, item := range instance.GetFinalizers() {
		if item == value {
			return true
		}
	}
	return false
}

func assertConditionStatus(t *testing.T, instance *platformv1alpha1.OGX, conditionType string, want metav1.ConditionStatus) {
	t.Helper()

	for _, condition := range instance.Status.Conditions {
		if condition.Type == conditionType {
			if condition.Status != want {
				t.Fatalf("condition %s status = %s, want %s", conditionType, condition.Status, want)
			}
			return
		}
	}

	t.Fatalf("condition %s not found", conditionType)
}

func writeComponentMetadata(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	componentDir := filepath.Join(root, componentName)
	if err := os.MkdirAll(componentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", componentDir, err)
	}

	content := []byte(`releases:
  - name: OGX
    version: v1.0.2
    repoUrl: https://github.com/opendatahub-io/ogx-distribution
  - name: OGX Operator
    version: v0.10.0
    repoUrl: https://github.com/opendatahub-io/ogx-k8s-operator
`)
	path := filepath.Join(componentDir, "component_metadata.yaml")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	return root
}

func writeMinimalManifestTree(t *testing.T) string {
	t.Helper()

	root := writeComponentMetadata(t)
	overlayDir := filepath.Join(root, componentName, overlayODH)
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", overlayDir, err)
	}

	content := []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
`)
	if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), content, 0o644); err != nil {
		t.Fatalf("WriteFile(kustomization.yaml): %v", err)
	}

	return root
}

func newOGXServerUnstructured(name, namespace string, conditions ...map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": ogxServerGVK.GroupVersion().String(),
			"kind":       ogxServerGVK.Kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"status": map[string]any{
				"conditions": toAnySlice(conditions),
			},
		},
	}
	obj.SetGroupVersionKind(ogxServerGVK)
	return obj
}

func newRenderedValidatingWebhook(name, serviceName, serviceNamespace string) unstructured.Unstructured {
	obj := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": admissionv1.SchemeGroupVersion.String(),
			"kind":       "ValidatingWebhookConfiguration",
			"metadata": map[string]any{
				"name": name,
			},
			"webhooks": []any{
				map[string]any{
					"name": "vogxserver.kb.io",
					"clientConfig": map[string]any{
						"service": map[string]any{
							"name":      serviceName,
							"namespace": serviceNamespace,
						},
					},
				},
			},
		},
	}
	obj.SetGroupVersionKind(admissionv1.SchemeGroupVersion.WithKind("ValidatingWebhookConfiguration"))
	return obj
}

func newRenderedWebhookDeployment(name, namespace, secretName string) unstructured.Unstructured {
	obj := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": appsv1.SchemeGroupVersion.String(),
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"volumes": []any{
							map[string]any{
								"name": "cert",
								"secret": map[string]any{
									"secretName": secretName,
								},
							},
						},
					},
				},
			},
		},
	}
	obj.SetGroupVersionKind(deploymentGVK)
	return obj
}

func condition(conditionType string, status metav1.ConditionStatus) map[string]any {
	return map[string]any{
		"type":   conditionType,
		"status": string(status),
	}
}

func newOGXServerUnstructuredNoConditions(name, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": ogxServerGVK.GroupVersion().String(),
			"kind":       ogxServerGVK.Kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"status": map[string]any{},
		},
	}
	obj.SetGroupVersionKind(ogxServerGVK)
	return obj
}

func TestResolveImageParamOverride(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		envVars   map[string]string
		wantValue string
		wantOK    bool
	}{
		{
			name:   "unknown key returns false",
			key:    "UNKNOWN_KEY",
			wantOK: false,
		},
		{
			name:      "valid image reference",
			key:       "RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE",
			envVars:   map[string]string{"RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE": "quay.io/opendatahub/odh-ogx-k8s-operator:v1.2.3"},
			wantValue: "quay.io/opendatahub/odh-ogx-k8s-operator:v1.2.3",
			wantOK:    true,
		},
		{
			name:      "valid image reference with digest",
			key:       "RELATED_IMAGE_ODH_OGX_CORE_IMAGE",
			envVars:   map[string]string{"RELATED_IMAGE_ODH_OGX_CORE_IMAGE": "quay.io/opendatahub/ogx-core@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"},
			wantValue: "quay.io/opendatahub/ogx-core@sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			wantOK:    true,
		},
		{
			name:   "empty env returns false",
			key:    "RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE",
			wantOK: false,
		},
		{
			name:    "invalid image reference is rejected",
			key:     "RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE",
			envVars: map[string]string{"RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE": "INVALID@@@image::ref"},
			wantOK:  false,
		},
		{
			name:    "image with newline is rejected",
			key:     "RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE",
			envVars: map[string]string{"RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE": "quay.io/repo/img:tag\nmalicious=injected"},
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			value, ok := resolveImageParamOverride(tt.key)
			if ok != tt.wantOK {
				t.Fatalf("resolveImageParamOverride() ok = %v, want %v", ok, tt.wantOK)
			}
			if value != tt.wantValue {
				t.Fatalf("resolveImageParamOverride() value = %q, want %q", value, tt.wantValue)
			}
		})
	}
}

func toAnySlice(items []map[string]any) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}
