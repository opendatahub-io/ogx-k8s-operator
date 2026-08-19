//nolint:testpackage
package e2e

import (
	"context"
	"testing"
	"time"

	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
	common "github.com/opendatahub-io/odh-platform-utilities/api/common"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runDeletionTests(t *testing.T, instance *platformv1alpha1.OGX) {
	t.Helper()

	t.Run("should remove root operator resources when managementState is Removed", func(t *testing.T) {
		testManagementStateRemoved(t, instance)
	})

	t.Run("should delete OGX CR after resources are removed", func(t *testing.T) {
		latest := getOGX(t)
		err := TestEnv.Client.Delete(TestEnv.Ctx, latest)
		if err != nil && !k8serrors.IsNotFound(err) {
			require.NoError(t, err)
		}

		err = EnsureResourceDeleted(t, TestEnv, schema.GroupVersionKind{
			Group:   platformv1alpha1.GroupVersion.Group,
			Version: platformv1alpha1.GroupVersion.Version,
			Kind:    platformv1alpha1.OGXKind,
		}, instance.Name, "", ResourceReadyTimeout)
		requireNoErrorWithDebugging(t, err, "OGX CR should be deleted")

		podList := &corev1.PodList{}
		err = TestEnv.Client.List(TestEnv.Ctx, podList,
			client.InNamespace(TestOpts.OperatorNS),
			client.MatchingLabels{"app.kubernetes.io/name": "ogx-k8s-operator"})
		require.NoError(t, err)
		require.Empty(t, podList.Items, "Found orphaned root operator pods")
	})
}

func testManagementStateRemoved(t *testing.T, instance *platformv1alpha1.OGX) {
	t.Helper()

	err := wait.PollUntilContextTimeout(TestEnv.Ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		latest := &platformv1alpha1.OGX{}
		if getErr := TestEnv.Client.Get(ctx, client.ObjectKey{Name: instance.Name}, latest); getErr != nil {
			return false, getErr
		}
		latest.Spec.ManagementState = common.Removed
		if updateErr := TestEnv.Client.Update(ctx, latest); updateErr != nil {
			if k8serrors.IsConflict(updateErr) {
				return false, nil
			}
			return false, updateErr
		}
		return true, nil
	})
	require.NoError(t, err, "Failed to set OGX managementState to Removed")

	err = EnsureResourceDeleted(t, TestEnv, schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	}, rootOperatorDeploymentName, TestOpts.OperatorNS, ResourceReadyTimeout)
	requireNoErrorWithDebugging(t, err, "Root operator deployment should be deleted when OGX is Removed")

	err = waitForOGXCondition(t, string(common.ConditionTypeProvisioningSucceeded), metav1.ConditionFalse, ResourceReadyTimeout)
	requireNoErrorWithDebugging(t, err, "OGX ProvisioningSucceeded should become False after Removed")

	err = wait.PollUntilContextTimeout(TestEnv.Ctx, pollInterval, ResourceReadyTimeout, true, func(ctx context.Context) (bool, error) {
		latest := &platformv1alpha1.OGX{}
		if getErr := TestEnv.Client.Get(ctx, client.ObjectKey{Name: instance.Name}, latest); getErr != nil {
			return false, getErr
		}
		return !containsFinalizer(latest, ogxFinalizer), nil
	})
	requireNoErrorWithDebugging(t, err, "OGX finalizer should be cleared after Removed cleanup")

	updated := getOGX(t)
	require.Equal(t, common.PhaseNotReady, updated.Status.Phase, "OGX phase should be NotReady after Removed")
	require.False(t, containsFinalizer(updated, ogxFinalizer), "OGX finalizer should be cleared after Removed cleanup")
}
