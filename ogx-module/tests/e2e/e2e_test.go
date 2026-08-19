//nolint:testpackage
package e2e

import (
	"testing"

	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
)

func TestE2E(t *testing.T) {
	registerSchemes()

	t.Run("validation", TestValidationSuite)

	var creationFailed bool
	var createdOGX *platformv1alpha1.OGX

	t.Run("creation-deletion", func(t *testing.T) {
		t.Run("creation", func(t *testing.T) {
			createdOGX = runCreationTests(t)
			creationFailed = t.Failed()
		})

		if creationFailed || TestOpts.SkipDeletion || createdOGX == nil {
			if TestOpts.SkipDeletion {
				t.Log("Skipping deletion tests (SkipDeletion=true)")
			} else {
				t.Log("Skipping deletion tests due to creation test failures")
			}
			return
		}

		t.Run("deletion", func(t *testing.T) {
			runDeletionTests(t, createdOGX)
		})
	})
}
