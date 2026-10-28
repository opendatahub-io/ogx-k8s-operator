//nolint:testpackage
package e2e

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestE2EDeploymentPodsReadyDuringRollout(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"app": "module"}
	deleting := metav1.Now()
	current := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "test", Labels: labels},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", Ready: true}},
		},
	}
	old := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "old", Namespace: "test", Labels: labels,
			DeletionTimestamp: &deleting, Finalizers: []string{"test.example/cleanup"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	previousEnv := TestEnv
	t.Cleanup(func() { TestEnv = previousEnv })
	TestEnv = &TestEnvironment{
		Ctx:    context.Background(),
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(current, old).Build(),
	}
	requireDeploymentPodsReady(t, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "module", Namespace: "test"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	})
}
