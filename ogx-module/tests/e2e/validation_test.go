//nolint:testpackage
package e2e

import (
	"testing"

	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestValidationSuite(t *testing.T) {
	if TestOpts.SkipValidation {
		t.Skip("Skipping validation test suite")
	}

	t.Run("should validate OGX CRD", func(t *testing.T) {
		err := validateCRD(TestEnv.Client, TestEnv.Ctx, ogxCRDName)
		require.NoErrorf(t, err, "error in validating CRD: %s", ogxCRDName)
	})

	t.Run("should validate module operator deployment status", func(t *testing.T) {
		requireDeploymentReady(t, moduleDeploymentName, TestOpts.OperatorNS)
	})

	t.Run("should reject OGX resources that are not named default-ogx", func(t *testing.T) {
		invalid := newManagedOGX()
		invalid.Name = "not-default-ogx"
		err := TestEnv.Client.Create(TestEnv.Ctx, invalid)
		require.Error(t, err, "OGX resources must be named %s", platformv1alpha1.OGXInstanceName)
	})
}

func containsFinalizer(instance *platformv1alpha1.OGX, value string) bool {
	for _, item := range instance.GetFinalizers() {
		if item == value {
			return true
		}
	}
	return false
}
