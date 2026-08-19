//nolint:testpackage
package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"testing"
	"time"

	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
	common "github.com/opendatahub-io/odh-platform-utilities/api/common"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	moduleOperatorNS           = "opendatahub-ogx-system"
	moduleDeploymentName       = "opendatahub-ogx-operator"
	rootOperatorDeploymentName = "ogx-k8s-operator-controller-manager"
	rootWebhookSecretName      = "ogx-k8s-operator-webhook-cert"
	ogxCRDName                 = "ogxs.components.platform.opendatahub.io"
	ogxServerCRDName           = "ogxservers.ogx.io"
	ogxFinalizer               = "components.platform.opendatahub.io/ogx-finalizer"
	conditionRootOperatorReady = "RootOperatorReady"
	conditionRootWebhookReady  = "RootWebhookReady"
	relatedImageOperatorEnv    = "RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE"
	pollInterval               = 10 * time.Second
	generalRetryInterval       = 5 * time.Second
	ResourceReadyTimeout       = 5 * time.Minute
)

var (
	Scheme = runtime.NewScheme()
)

// TestEnvironment holds the test environment configuration.
type TestEnvironment struct {
	Client client.Client
	Ctx    context.Context //nolint:containedctx // Context is used for test environment
}

// SetupTestEnv sets up the test environment.
func SetupTestEnv() (*TestEnvironment, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	cl, err := client.New(cfg, client.Options{Scheme: Scheme})
	if err != nil {
		return nil, err
	}

	return &TestEnvironment{
		Client: cl,
		Ctx:    context.TODO(),
	}, nil
}

// CleanupTestEnv cleans up the test environment.
func CleanupTestEnv(_ *TestEnvironment) {
}

func validateCRD(c client.Client, ctx context.Context, crdName string) error {
	crd := &apiextv1.CustomResourceDefinition{}
	obj := client.ObjectKey{Name: crdName}

	return wait.PollUntilContextTimeout(ctx, generalRetryInterval, ResourceReadyTimeout, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, obj, crd)
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			log.Printf("Failed to get CRD %s", crdName)
			return false, err
		}

		for _, condition := range crd.Status.Conditions {
			if condition.Type == apiextv1.Established && condition.Status == apiextv1.ConditionTrue {
				return true, nil
			}
		}
		log.Printf("Error to get CRD %s condition's matching", crdName)
		return false, nil
	})
}

// GetDeployment gets a deployment by name and namespace.
func GetDeployment(cl client.Client, ctx context.Context, name, namespace string) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{}
	err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, deployment)
	return deployment, err
}

// EnsureResourceReady polls until the resource is ready.
func EnsureResourceReady(
	t *testing.T,
	testenv *TestEnvironment,
	gvk schema.GroupVersionKind,
	name, namespace string,
	timeout time.Duration,
	isReady func(*unstructured.Unstructured) bool,
) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(testenv.Ctx, timeout)
	defer cancel()
	return wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		err := testenv.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj)
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return isReady(obj), nil
	})
}

// EnsureResourceDeleted polls until the resource is deleted.
func EnsureResourceDeleted(t *testing.T, testenv *TestEnvironment, gvk schema.GroupVersionKind, name, namespace string, timeout time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(testenv.Ctx, timeout)
	defer cancel()
	return wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		err := testenv.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, obj)
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, nil
	})
}

func isDeploymentReady(u *unstructured.Unstructured) bool {
	generation, _, _ := unstructured.NestedInt64(u.Object, "metadata", "generation")
	observedGeneration, foundObserved, err := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
	if err != nil || (foundObserved && observedGeneration < generation) {
		return false
	}

	expected := int64(1)
	if replicas, found, specErr := unstructured.NestedInt64(u.Object, "spec", "replicas"); specErr == nil && found && replicas > 0 {
		expected = replicas
	}

	readyReplicas, foundReady, err := unstructured.NestedInt64(u.Object, "status", "readyReplicas")
	if !foundReady || err != nil {
		return false
	}
	availableReplicas, foundAvailable, err := unstructured.NestedInt64(u.Object, "status", "availableReplicas")
	if !foundAvailable || err != nil {
		return false
	}

	return readyReplicas >= expected && availableReplicas >= expected
}

func requireDeploymentReady(t *testing.T, name, namespace string) *appsv1.Deployment {
	t.Helper()

	deployment, err := GetDeployment(TestEnv.Client, TestEnv.Ctx, name, namespace)
	require.NoError(t, err, "Deployment %s/%s not found", namespace, name)

	expected := int32(1)
	if deployment.Spec.Replicas != nil {
		expected = *deployment.Spec.Replicas
	}

	require.GreaterOrEqual(t, deployment.Status.ObservedGeneration, deployment.Generation,
		"Deployment %s/%s has not observed the latest generation", namespace, name)
	require.GreaterOrEqual(t, deployment.Status.ReadyReplicas, expected,
		"Deployment %s/%s ReadyReplicas=%d want >= %d", namespace, name, deployment.Status.ReadyReplicas, expected)
	require.GreaterOrEqual(t, deployment.Status.AvailableReplicas, expected,
		"Deployment %s/%s AvailableReplicas=%d want >= %d", namespace, name, deployment.Status.AvailableReplicas, expected)
	require.GreaterOrEqual(t, deployment.Status.UpdatedReplicas, expected,
		"Deployment %s/%s UpdatedReplicas=%d want >= %d", namespace, name, deployment.Status.UpdatedReplicas, expected)
	require.True(t, isDeploymentAvailable(deployment),
		"Deployment %s/%s should have Available=True", namespace, name)

	requireDeploymentPodsReady(t, deployment)
	return deployment
}

func isDeploymentAvailable(deployment *appsv1.Deployment) bool {
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func requireDeploymentPodsReady(t *testing.T, deployment *appsv1.Deployment) {
	t.Helper()

	podList := &corev1.PodList{}
	err := TestEnv.Client.List(TestEnv.Ctx, podList,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels(deployment.Spec.Selector.MatchLabels))
	require.NoError(t, err, "Failed to list pods for deployment %s/%s", deployment.Namespace, deployment.Name)
	require.NotEmpty(t, podList.Items, "No pods found for deployment %s/%s", deployment.Namespace, deployment.Name)

	for _, pod := range podList.Items {
		require.Equal(t, corev1.PodRunning, pod.Status.Phase, "Pod %s is not running", pod.Name)
		require.True(t, isPodReady(&pod), "Pod %s is not ready", pod.Name)
		for _, cs := range pod.Status.ContainerStatuses {
			require.True(t, cs.Ready, "Container %s in pod %s is not ready", cs.Name, pod.Name)
		}
	}
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func registerSchemes() {
	schemes := []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		apiextv1.AddToScheme,
		platformv1alpha1.AddToScheme,
	}

	for _, schemeFn := range schemes {
		utilruntime.Must(schemeFn(Scheme))
	}
}

func newManagedOGX() *platformv1alpha1.OGX {
	return &platformv1alpha1.OGX{
		TypeMeta: metav1.TypeMeta{
			APIVersion: platformv1alpha1.GroupVersion.String(),
			Kind:       platformv1alpha1.OGXKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: platformv1alpha1.OGXInstanceName,
		},
		Spec: platformv1alpha1.OGXSpec{
			ManagementSpec: common.ManagementSpec{ManagementState: common.Managed},
		},
	}
}

func getOGX(t *testing.T) *platformv1alpha1.OGX {
	t.Helper()
	instance := &platformv1alpha1.OGX{}
	err := TestEnv.Client.Get(TestEnv.Ctx, client.ObjectKey{Name: platformv1alpha1.OGXInstanceName}, instance)
	require.NoError(t, err, "OGX CR %s should exist", platformv1alpha1.OGXInstanceName)
	return instance
}

func waitForOGXCondition(t *testing.T, conditionType string, want metav1.ConditionStatus, timeout time.Duration) error {
	t.Helper()
	return wait.PollUntilContextTimeout(TestEnv.Ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		instance := &platformv1alpha1.OGX{}
		err := TestEnv.Client.Get(ctx, client.ObjectKey{Name: platformv1alpha1.OGXInstanceName}, instance)
		if err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		for _, condition := range instance.Status.Conditions {
			if condition.Type == conditionType {
				t.Logf("OGX condition %s=%s reason=%s message=%s phase=%s",
					condition.Type, condition.Status, condition.Reason, condition.Message, instance.Status.Phase)
				return condition.Status == want, nil
			}
		}

		t.Logf("OGX condition %s not present yet; phase=%s conditions=%+v", conditionType, instance.Status.Phase, instance.Status.Conditions)
		return false, nil
	})
}

func envValue(deployment *appsv1.Deployment, name string) string {
	for i := range deployment.Spec.Template.Spec.Containers {
		container := deployment.Spec.Template.Spec.Containers[i]
		if container.Name != "manager" {
			continue
		}
		for _, env := range container.Env {
			if env.Name == name {
				return env.Value
			}
		}
	}
	return ""
}

func ensureWebhookCertSecret(t *testing.T, namespace string) {
	t.Helper()

	certPEM, keyPEM, err := generateSelfSignedCert(namespace)
	require.NoError(t, err, "failed to generate webhook certificate")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rootWebhookSecretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}

	err = TestEnv.Client.Create(TestEnv.Ctx, secret)
	if err != nil && !errors.IsAlreadyExists(err) {
		require.NoError(t, err, "failed to create webhook certificate secret")
	}
}

func generateSelfSignedCert(namespace string) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "ogx-k8s-operator-webhook-service",
		},
		DNSNames: []string{
			"ogx-k8s-operator-webhook-service",
			"ogx-k8s-operator-webhook-service." + namespace,
			"ogx-k8s-operator-webhook-service." + namespace + ".svc",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

func requireNoErrorWithDebugging(t *testing.T, err error, msg string) {
	t.Helper()
	if err == nil {
		return
	}

	t.Logf("ERROR OCCURRED: %s - %v", msg, err)
	logOGXStatus(t)
	logNamespaceEvents(t, TestOpts.OperatorNS)
	logPodDetails(t, TestOpts.OperatorNS)
	logDeploymentSpec(t, TestOpts.OperatorNS, moduleDeploymentName)
	logDeploymentSpec(t, TestOpts.OperatorNS, rootOperatorDeploymentName)

	require.NoError(t, err, msg)
}

func logOGXStatus(t *testing.T) {
	t.Helper()

	instance := &platformv1alpha1.OGX{}
	err := TestEnv.Client.Get(TestEnv.Ctx, client.ObjectKey{Name: platformv1alpha1.OGXInstanceName}, instance)
	if err != nil {
		t.Logf("Error getting OGX: %v", err)
		return
	}

	t.Logf("OGX status:")
	t.Logf("  Phase: %s", instance.Status.Phase)
	t.Logf("  Generation: %d", instance.Generation)
	t.Logf("  ObservedGeneration: %d", instance.Status.ObservedGeneration)
	t.Logf("  Distribution: %+v", instance.Status.Distribution)
	t.Logf("  Conditions: %+v", instance.Status.Conditions)
}

func logNamespaceEvents(t *testing.T, namespace string) {
	t.Helper()

	eventList := &corev1.EventList{}
	err := TestEnv.Client.List(TestEnv.Ctx, eventList, client.InNamespace(namespace))
	if err != nil {
		t.Logf("Error getting events: %v", err)
		return
	}

	maxEvents := 25
	if len(eventList.Items) > maxEvents {
		t.Logf("Showing first %d events (of %d total):", maxEvents, len(eventList.Items))
		eventList.Items = eventList.Items[:maxEvents]
	} else {
		t.Logf("Found %d events in namespace %s:", len(eventList.Items), namespace)
	}

	for _, event := range eventList.Items {
		t.Logf("  %s: %s (%s) - %s",
			event.LastTimestamp.Format("15:04:05"),
			event.Reason,
			event.Type,
			event.Message)
	}
}

func logPodDetails(t *testing.T, namespace string) {
	t.Helper()

	podList := &corev1.PodList{}
	err := TestEnv.Client.List(TestEnv.Ctx, podList, client.InNamespace(namespace))
	if err != nil {
		t.Logf("Failed to list pods: %v", err)
		return
	}

	t.Logf("Found %d pods in namespace %s:", len(podList.Items), namespace)
	for _, pod := range podList.Items {
		t.Logf("Pod: %s, Phase: %s", pod.Name, pod.Status.Phase)
		for _, cs := range pod.Status.ContainerStatuses {
			t.Logf("  Container %s: Ready=%v, RestartCount=%d", cs.Name, cs.Ready, cs.RestartCount)
			if cs.State.Waiting != nil {
				t.Logf("    Waiting: %s - %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
			if cs.State.Terminated != nil {
				t.Logf("    Terminated: %s - %s", cs.State.Terminated.Reason, cs.State.Terminated.Message)
			}
		}
	}
}

func logDeploymentSpec(t *testing.T, namespace, name string) {
	t.Helper()

	deployment := &appsv1.Deployment{}
	err := TestEnv.Client.Get(TestEnv.Ctx, types.NamespacedName{Name: name, Namespace: namespace}, deployment)
	if err != nil {
		t.Logf("Failed to get deployment %s: %v", name, err)
		return
	}

	t.Logf("Deployment %s spec:", name)
	t.Logf("  Replicas: %d", *deployment.Spec.Replicas)
	t.Logf("  ReadyReplicas: %d", deployment.Status.ReadyReplicas)
	for _, container := range deployment.Spec.Template.Spec.Containers {
		t.Logf("  Container: %s image=%s", container.Name, container.Image)
	}
}
