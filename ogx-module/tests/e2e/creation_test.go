//nolint:testpackage
package e2e

import (
	"testing"

	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
	common "github.com/opendatahub-io/odh-platform-utilities/api/common"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runCreationTests(t *testing.T) *platformv1alpha1.OGX {
	t.Helper()
	if TestOpts.SkipCreation {
		t.Skip("Skipping creation test suite")
	}

	var instance *platformv1alpha1.OGX

	t.Run("should create OGX", func(t *testing.T) {
		instance = testCreateOGX(t)
	})

	requireOGX := func(t *testing.T) {
		t.Helper()
		if instance == nil {
			t.Skip("Skipping: OGX creation failed")
		}
	}

	t.Run("should add the OGX finalizer", func(t *testing.T) {
		requireOGX(t)
		updated := getOGX(t)
		require.True(t, containsFinalizer(updated, ogxFinalizer), "OGX should have finalizer %s", ogxFinalizer)
	})

	t.Run("should deploy the root OGX operator with a ready status", func(t *testing.T) {
		requireOGX(t)
		testRootOperatorDeployment(t)
	})

	t.Run("should establish the OGXServer CRD", func(t *testing.T) {
		requireOGX(t)
		err := validateCRD(TestEnv.Client, TestEnv.Ctx, ogxServerCRDName)
		requireNoErrorWithDebugging(t, err, "OGXServer CRD should be established")
	})

	t.Run("should report ready status conditions", func(t *testing.T) {
		requireOGX(t)
		testOGXReadyStatus(t)
	})

	t.Run("should report OpenDataHub distribution status", func(t *testing.T) {
		requireOGX(t)
		updated := getOGX(t)
		require.Equal(t, "OpenDataHub", updated.Status.Distribution.Name, "Distribution name should match the module platform")
		require.NotEmpty(t, updated.Status.Distribution.Version, "Distribution version should be populated")
	})

	t.Run("should apply root operator image override from the module operator", func(t *testing.T) {
		requireOGX(t)
		testRootOperatorImageOverride(t)
	})

	return instance
}

func testCreateOGX(t *testing.T) *platformv1alpha1.OGX {
	t.Helper()

	ensureWebhookCertSecret(t, TestOpts.OperatorNS)

	instance := newManagedOGX()
	t.Logf("Creating OGX %s", instance.Name)

	err := TestEnv.Client.Create(TestEnv.Ctx, instance)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}

	return getOGX(t)
}

func testRootOperatorDeployment(t *testing.T) {
	t.Helper()

	err := EnsureResourceReady(t, TestEnv, schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	}, rootOperatorDeploymentName, TestOpts.OperatorNS, ResourceReadyTimeout, isDeploymentReady)
	requireNoErrorWithDebugging(t, err, "Root OGX operator deployment should become ready")

	requireDeploymentReady(t, rootOperatorDeploymentName, TestOpts.OperatorNS)
}

func testOGXReadyStatus(t *testing.T) {
	t.Helper()

	err := waitForOGXCondition(t, string(common.ConditionTypeProvisioningSucceeded), metav1.ConditionTrue, ResourceReadyTimeout)
	requireNoErrorWithDebugging(t, err, "OGX ProvisioningSucceeded should become True")

	err = waitForOGXCondition(t, conditionRootOperatorReady, metav1.ConditionTrue, ResourceReadyTimeout)
	requireNoErrorWithDebugging(t, err, "OGX RootOperatorReady should become True")

	err = waitForOGXCondition(t, conditionRootWebhookReady, metav1.ConditionTrue, ResourceReadyTimeout)
	requireNoErrorWithDebugging(t, err, "OGX RootWebhookReady should become True")

	err = waitForOGXCondition(t, string(common.ConditionTypeReady), metav1.ConditionTrue, ResourceReadyTimeout)
	requireNoErrorWithDebugging(t, err, "OGX Ready should become True")

	updated := getOGX(t)
	require.Equal(t, common.PhaseReady, updated.Status.Phase, "OGX phase should be Ready")
}

func testRootOperatorImageOverride(t *testing.T) {
	t.Helper()

	moduleDeployment := &appsv1.Deployment{}
	err := TestEnv.Client.Get(TestEnv.Ctx, client.ObjectKey{
		Namespace: TestOpts.OperatorNS,
		Name:      moduleDeploymentName,
	}, moduleDeployment)
	require.NoError(t, err)

	overrideImage := envValue(moduleDeployment, relatedImageOperatorEnv)
	if overrideImage == "" {
		t.Skip("Skipping image override check because RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE is unset")
	}

	rootDeployment := &appsv1.Deployment{}
	err = TestEnv.Client.Get(TestEnv.Ctx, client.ObjectKey{
		Namespace: TestOpts.OperatorNS,
		Name:      rootOperatorDeploymentName,
	}, rootDeployment)
	require.NoError(t, err)
	require.NotEmpty(t, rootDeployment.Spec.Template.Spec.Containers, "Root operator deployment should have containers")
	require.Equal(t, overrideImage, rootDeployment.Spec.Template.Spec.Containers[0].Image,
		"Root operator image should match the module operator override")
}
