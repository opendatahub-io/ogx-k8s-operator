package ogx

import (
	"context"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/distribution/reference"
	platformv1alpha1 "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/apis/v1alpha1"
	moduleconfig "github.com/ogx-ai/ogx-k8s-operator/ogx-module/pkg/config"
	common "github.com/opendatahub-io/odh-platform-utilities/api/common"
	odhconditions "github.com/opendatahub-io/odh-platform-utilities/pkg/controller/conditions"
	odhgc "github.com/opendatahub-io/odh-platform-utilities/pkg/controller/gc"
	odhdeploy "github.com/opendatahub-io/odh-platform-utilities/pkg/deploy"
	odhlabels "github.com/opendatahub-io/odh-platform-utilities/pkg/metadata/labels"
	odhkustomize "github.com/opendatahub-io/odh-platform-utilities/pkg/render/kustomize"
	"gopkg.in/yaml.v3"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const (
	componentName                  = platformv1alpha1.OGXComponentName
	overlayODH                     = "overlays/odh"
	overlayRhoai                   = "overlays/rhoai"
	paramsFileName                 = "params.env"
	ogxFinalizer                   = "components.platform.opendatahub.io/ogx-finalizer"
	conditionTypeRootOperatorReady = "RootOperatorReady"
	conditionTypeRootWebhookReady  = "RootWebhookReady"
	ogxServerDeploymentReady       = "DeploymentReady"
	ogxServerHealthCheck           = "HealthCheck"

	// platformConfigCMName is the ODH-managed ConfigMap (odh-<module>-config)
	// that carries data.platformVersion for the version handshake.
	platformConfigCMName = "odh-" + componentName + "-config"
	// platformVersionDataKey is the camelCase key injected by the platform
	// operator. The kebab-case config key is accepted as a fallback.
	platformVersionDataKey = "platformVersion"
	platformReleaseName    = "platform"
)

var (
	deploymentGVK    = appsv1.SchemeGroupVersion.WithKind("Deployment")
	crdGVK           = extv1.SchemeGroupVersion.WithKind("CustomResourceDefinition")
	namespaceGVK     = corev1.SchemeGroupVersion.WithKind("Namespace")
	ogxServerGVK     = schema.GroupVersionKind{Group: "ogx.io", Version: "v1beta1", Kind: "OGXServer"}
	imageParamEnvMap = map[string][]string{
		"RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE": {"RELATED_IMAGE_ODH_OGX_K8S_OPERATOR_IMAGE"},
		"RELATED_IMAGE_ODH_OGX_CORE_IMAGE":         {"RELATED_IMAGE_ODH_OGX_CORE_IMAGE"},
	}
)

type componentMetadata struct {
	Releases []common.ComponentRelease `yaml:"releases"`
}

type gcRunner interface {
	Run(context.Context, odhgc.RunParams) error
}

type ogxServerHealthSummary struct {
	Total          int
	UnhealthyCount int
	Unhealthy      []string
}

// Reconciler manages the ODH-facing OGX CR and deploys the staged root OGX
// operator manifests.
type Reconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	APIReader        client.Reader
	Config           *moduleconfig.Config
	Deployer         *odhdeploy.Deployer
	DynamicClient    dynamic.Interface
	DiscoveryClient  discovery.DiscoveryInterface
	GarbageCollector gcRunner
}

func NewReconciler(
	cli client.Client,
	apiReader client.Reader,
	scheme *runtime.Scheme,
	cfg *moduleconfig.Config,
	dynamicCli dynamic.Interface,
	discoveryCli discovery.DiscoveryInterface,
) *Reconciler {
	return &Reconciler{
		Client:          cli,
		APIReader:       apiReader,
		Scheme:          scheme,
		Config:          cfg,
		DynamicClient:   dynamicCli,
		DiscoveryClient: discoveryCli,
		Deployer: odhdeploy.NewDeployer(
			odhdeploy.WithApplyOrder(),
			odhdeploy.WithCache(),
			odhdeploy.WithLabel(odhlabels.ManagedBy, componentName),
			odhdeploy.WithMergeStrategy(deploymentGVK, odhdeploy.MergeDeployments),
		),
		GarbageCollector: odhgc.New(
			odhgc.InNamespace(cfg.ApplicationsNamespace),
			odhgc.WithObjectPredicate(func(_ odhgc.RunParams, _ unstructured.Unstructured) (bool, error) {
				return true, nil
			}),
		),
	}
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.OGX{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&rbacv1.ClusterRole{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Owns(&admissionv1.ValidatingWebhookConfiguration{}).
		Owns(&extv1.CustomResourceDefinition{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{Name: platformv1alpha1.OGXInstanceName},
				}}
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(r.isPlatformConfigMap)),
		).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("name", req.Name)
	ctx = log.IntoContext(ctx, logger)

	instance := &platformv1alpha1.OGX{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if managementState(instance) == common.Managed {
		if changed := ensureFinalizer(instance); changed {
			if err := r.Update(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if err := r.refreshPlatformVersion(ctx); err != nil {
		r.updateStatusOnError(ctx, instance, false, false, false, err, "failed to update status after platform config error")
		return ctrl.Result{}, err
	}

	rendered, err := r.renderRootOperatorResources()
	if err != nil {
		r.updateStatusOnError(ctx, instance, false, false, false, err, "failed to update status after render error")
		return ctrl.Result{}, err
	}

	if !instance.DeletionTimestamp.IsZero() || managementState(instance) == common.Removed {
		err = r.cleanupRootOperatorResources(ctx, instance)
		if statusErr := r.updateStatus(ctx, instance, false, false, false, ogxServerHealthSummary{}, err); statusErr != nil {
			logger.Error(statusErr, "failed to update removed status")
			if err == nil {
				err = statusErr
			}
		}

		if err == nil && controllerutil.ContainsFinalizer(instance, ogxFinalizer) {
			clearFinalizer(instance)
			if updateErr := r.Update(ctx, instance); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
		}

		return ctrl.Result{}, err
	}

	err = r.Deployer.Deploy(ctx, odhdeploy.DeployInput{
		Client:    r.Client,
		Owner:     instance,
		Release:   odhdeploy.ReleaseInfo{Type: r.Config.PlatformName, Version: r.Config.PlatformVersion},
		Resources: rendered,
	})
	if err != nil {
		r.updateStatusOnError(ctx, instance, false, false, false, err, "failed to update status after deploy error")
		return ctrl.Result{}, err
	}

	deploymentsReady, readyErr := r.rootDeploymentsReady(ctx, rendered)
	if readyErr != nil {
		r.updateStatusOnError(ctx, instance, true, false, false, readyErr, "failed to update status after readiness error")
		return ctrl.Result{}, readyErr
	}

	webhookReady, webhookErr := r.rootWebhookResourcesReady(ctx, rendered)
	if webhookErr != nil {
		r.updateStatusOnError(ctx, instance, true, deploymentsReady, false, webhookErr, "failed to update status after webhook readiness error")
		return ctrl.Result{}, webhookErr
	}

	healthSummary, healthErr := r.aggregateOGXServerHealth(ctx)
	if healthErr != nil {
		r.updateStatusOnError(ctx, instance, true, deploymentsReady, webhookReady, healthErr, "failed to update status after OGXServer health error")
		return ctrl.Result{}, healthErr
	}

	if err := r.updateStatus(ctx, instance, true, deploymentsReady, webhookReady, healthSummary, nil); err != nil {
		return ctrl.Result{}, err
	}

	if !deploymentsReady || !webhookReady {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) renderRootOperatorResources() ([]unstructured.Unstructured, error) {
	overlay := overlayForPlatform(r.Config.PlatformName)
	componentRoot := filepath.Join(r.Config.ManifestsPath, componentName)
	overlayPath := filepath.Join(componentRoot, overlay)
	engineOpts := []odhkustomize.EngineOptsFn{}

	if hasImageParamOverrides() {
		renderFS, renderPath, err := prepareManifestFSWithOverrides(componentRoot, overlay)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare manifest filesystem with image overrides: %w", err)
		}

		engineOpts = append(engineOpts, odhkustomize.WithEngineFS(renderFS))
		overlayPath = renderPath
	}

	rendered, err := odhkustomize.Render(
		overlayPath,
		engineOpts,
		odhkustomize.WithNamespace(r.Config.ApplicationsNamespace),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to render root operator manifests from %s: %w", overlayPath, err)
	}

	filtered := make([]unstructured.Unstructured, 0, len(rendered))
	for i := range rendered {
		obj := *rendered[i].DeepCopy()
		switch obj.GroupVersionKind() {
		case namespaceGVK:
			// The module bootstrap already owns the namespace; do not let the
			// staged root operator bundle create or rename it.
			continue
		case crdGVK:
			obj.SetNamespace("")
		}

		filtered = append(filtered, obj)
	}

	return filtered, nil
}

func (r *Reconciler) cleanupRootOperatorResources(ctx context.Context, instance *platformv1alpha1.OGX) error {
	if r.GarbageCollector == nil {
		return fmt.Errorf("failed to run cleanup because garbage collector is not configured")
	}

	return r.GarbageCollector.Run(ctx, odhgc.RunParams{
		Client:          r.Client,
		DynamicClient:   r.DynamicClient,
		DiscoveryClient: r.DiscoveryClient,
		Owner:           instance,
		Version:         r.Config.PlatformVersion,
		PlatformType:    r.Config.PlatformName,
	})
}

func (r *Reconciler) rootDeploymentsReady(ctx context.Context, resources []unstructured.Unstructured) (bool, error) {
	deployments := make([]client.ObjectKey, 0)

	for i := range resources {
		if resources[i].GroupVersionKind() != deploymentGVK {
			continue
		}
		deployments = append(deployments, client.ObjectKeyFromObject(&resources[i]))
	}

	for i := range deployments {
		deployment := &appsv1.Deployment{}
		if err := r.Get(ctx, deployments[i], deployment); err != nil {
			return false, fmt.Errorf("failed to get deployment %s: %w", deployments[i].String(), err)
		}

		if deployment.Generation > deployment.Status.ObservedGeneration {
			return false, nil
		}

		expected := int32(1)
		if deployment.Spec.Replicas != nil {
			expected = *deployment.Spec.Replicas
		}

		if deployment.Status.ReadyReplicas < expected || deployment.Status.AvailableReplicas < expected {
			return false, nil
		}
	}

	return true, nil
}

func (r *Reconciler) rootWebhookResourcesReady(ctx context.Context, resources []unstructured.Unstructured) (bool, error) {
	webhookNames := make([]string, 0)
	webhookSecretRefs := make([]client.ObjectKey, 0)
	for i := range resources {
		switch resources[i].GroupVersionKind() {
		case admissionv1.SchemeGroupVersion.WithKind("ValidatingWebhookConfiguration"):
			webhookNames = append(webhookNames, resources[i].GetName())
		case deploymentGVK:
			secretRefs, err := webhookSecretRefsFromDeployment(&resources[i])
			if err != nil {
				return false, err
			}
			webhookSecretRefs = append(webhookSecretRefs, secretRefs...)
		}
	}

	if len(webhookNames) == 0 {
		return true, nil
	}

	for _, name := range webhookNames {
		vwc := &admissionv1.ValidatingWebhookConfiguration{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, vwc); err != nil {
			switch {
			case apierrors.IsNotFound(err):
				return false, nil
			default:
				return false, fmt.Errorf("failed to get validating webhook configuration %s: %w", name, err)
			}
		}

		for _, webhook := range vwc.Webhooks {
			if webhook.ClientConfig.Service == nil {
				continue
			}

			serviceKey := client.ObjectKey{
				Name:      webhook.ClientConfig.Service.Name,
				Namespace: webhook.ClientConfig.Service.Namespace,
			}
			service := &corev1.Service{}
			if err := r.Get(ctx, serviceKey, service); err != nil {
				switch {
				case apierrors.IsNotFound(err):
					return false, nil
				default:
					return false, fmt.Errorf("failed to get webhook service %s: %w", serviceKey.String(), err)
				}
			}
		}
	}

	for _, secretKey := range webhookSecretRefs {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			switch {
			case apierrors.IsNotFound(err):
				return false, nil
			default:
				return false, fmt.Errorf("failed to get webhook secret %s: %w", secretKey.String(), err)
			}
		}
	}

	return true, nil
}

func webhookSecretRefsFromDeployment(obj *unstructured.Unstructured) ([]client.ObjectKey, error) {
	volumes, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		return nil, err
	}

	refs := make([]client.ObjectKey, 0)
	for i := range volumes {
		volume, ok := volumes[i].(map[string]any)
		if !ok {
			continue
		}

		secret, ok := volume["secret"].(map[string]any)
		if !ok {
			continue
		}

		secretName, _ := secret["secretName"].(string)
		if secretName == "" {
			continue
		}

		refs = append(refs, client.ObjectKey{
			Name:      secretName,
			Namespace: obj.GetNamespace(),
		})
	}

	return refs, nil
}

func (r *Reconciler) aggregateOGXServerHealth(ctx context.Context) (ogxServerHealthSummary, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ogxServerGVK.GroupVersion().WithKind("OGXServerList"))

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	if err := reader.List(ctx, list); err != nil {
		switch {
		case apimeta.IsNoMatchError(err), apierrors.IsNotFound(err):
			return ogxServerHealthSummary{}, nil
		default:
			return ogxServerHealthSummary{}, fmt.Errorf("failed to list OGXServer instances: %w", err)
		}
	}

	summary := ogxServerHealthSummary{
		Total: len(list.Items),
	}

	for i := range list.Items {
		obj := &list.Items[i]
		if !ogxServerIsHealthy(obj) {
			summary.UnhealthyCount++
			summary.Unhealthy = append(summary.Unhealthy, fmt.Sprintf("%s/%s", obj.GetNamespace(), obj.GetName()))
		}
	}

	return summary, nil
}

func ogxServerIsHealthy(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}

	deploymentReadyFalse := hasConditionStatus(conditions, ogxServerDeploymentReady, metav1.ConditionFalse)
	healthCheckFalse := hasConditionStatus(conditions, ogxServerHealthCheck, metav1.ConditionFalse)

	return !deploymentReadyFalse && !healthCheckFalse
}

func hasConditionStatus(conditions []interface{}, conditionType string, status metav1.ConditionStatus) bool {
	for i := range conditions {
		item, ok := conditions[i].(map[string]any)
		if !ok {
			continue
		}

		currentType, _ := item["type"].(string)
		currentStatus, _ := item["status"].(string)
		if currentType == conditionType && currentStatus == string(status) {
			return true
		}
	}

	return false
}

func (r *Reconciler) updateStatus(
	ctx context.Context,
	instance *platformv1alpha1.OGX,
	provisioningSucceeded bool,
	rootOperatorReady bool,
	rootWebhookReady bool,
	healthSummary ogxServerHealthSummary,
	reconcileErr error,
) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
		return client.IgnoreNotFound(err)
	}

	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.Distribution = platformv1alpha1.OGXDistribution{
		Name:    r.Config.PlatformName,
		Version: r.platformVersion(),
	}

	releases, err := r.loadComponentReleases()
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to load component metadata")
	} else {
		instance.Status.ComponentReleaseStatus = releases
	}

	setPlatformRelease(&instance.Status.ComponentReleaseStatus, r.platformVersion())

	cm := odhconditions.NewManager(
		instance,
		string(common.ConditionTypeReady),
		string(common.ConditionTypeProvisioningSucceeded),
		conditionTypeRootOperatorReady,
		conditionTypeRootWebhookReady,
	)
	observed := odhconditions.WithObservedGeneration(instance.Generation)

	switch {
	case managementState(instance) == common.Removed:
		cm.MarkFalse(
			string(common.ConditionTypeProvisioningSucceeded),
			observed,
			odhconditions.WithReason("Removed"),
			odhconditions.WithMessage("OGX module resources were removed"),
		)
		cm.MarkFalse(
			conditionTypeRootOperatorReady,
			observed,
			odhconditions.WithReason("Removed"),
			odhconditions.WithMessage("Root OGX operator deployment is removed"),
		)
		cm.MarkFalse(
			conditionTypeRootWebhookReady,
			observed,
			odhconditions.WithReason("Removed"),
			odhconditions.WithMessage("Root OGX operator webhook resources are removed"),
		)
		cm.MarkFalse(
			string(common.ConditionTypeDegraded),
			observed,
			odhconditions.WithReason("Removed"),
			odhconditions.WithMessage("OGX module is not degraded"),
		)
	case reconcileErr != nil:
		cm.MarkFalse(
			string(common.ConditionTypeProvisioningSucceeded),
			observed,
			odhconditions.WithReason("DeployFailed"),
			odhconditions.WithError(reconcileErr),
		)
		cm.MarkFalse(
			conditionTypeRootOperatorReady,
			observed,
			odhconditions.WithReason("DeployFailed"),
			odhconditions.WithMessage("Root OGX operator deployment is not ready"),
		)
		cm.MarkFalse(
			conditionTypeRootWebhookReady,
			observed,
			odhconditions.WithReason("DeployFailed"),
			odhconditions.WithMessage("Root OGX operator webhook resources are not ready"),
		)
		cm.MarkFalse(
			string(common.ConditionTypeDegraded),
			observed,
			odhconditions.WithReason("DeployFailed"),
			odhconditions.WithMessage("OGX module deploy failed"),
		)
	case rootOperatorReady:
		cm.MarkTrue(
			string(common.ConditionTypeProvisioningSucceeded),
			observed,
			odhconditions.WithReason("Provisioned"),
			odhconditions.WithMessage("Root OGX operator manifests applied successfully"),
		)
		cm.MarkTrue(
			conditionTypeRootOperatorReady,
			observed,
			odhconditions.WithReason("DeploymentReady"),
			odhconditions.WithMessage("Root OGX operator deployment is ready"),
		)
		if rootWebhookReady {
			cm.MarkTrue(
				conditionTypeRootWebhookReady,
				observed,
				odhconditions.WithReason("WebhookReady"),
				odhconditions.WithMessage("Root OGX operator webhook resources are ready"),
			)
		} else {
			cm.MarkFalse(
				conditionTypeRootWebhookReady,
				observed,
				odhconditions.WithReason("WebhookNotReady"),
				odhconditions.WithMessage("Root OGX operator webhook resources are not ready"),
			)
		}
	case provisioningSucceeded:
		cm.MarkTrue(
			string(common.ConditionTypeProvisioningSucceeded),
			observed,
			odhconditions.WithReason("Provisioned"),
			odhconditions.WithMessage("Root OGX operator manifests applied successfully"),
		)
		cm.MarkFalse(
			conditionTypeRootOperatorReady,
			observed,
			odhconditions.WithReason("Deploying"),
			odhconditions.WithMessage("Waiting for root OGX operator deployment to become ready"),
		)
		cm.MarkFalse(
			conditionTypeRootWebhookReady,
			observed,
			odhconditions.WithReason("Deploying"),
			odhconditions.WithMessage("Waiting for root OGX operator webhook resources to become ready"),
		)
	default:
		cm.MarkFalse(
			string(common.ConditionTypeProvisioningSucceeded),
			observed,
			odhconditions.WithReason("Unknown"),
			odhconditions.WithMessage("OGX module provisioning state is unknown"),
		)
		cm.MarkUnknown(
			conditionTypeRootOperatorReady,
			observed,
			odhconditions.WithReason("Unknown"),
			odhconditions.WithMessage("OGX module state is unknown"),
		)
		cm.MarkUnknown(
			conditionTypeRootWebhookReady,
			observed,
			odhconditions.WithReason("Unknown"),
			odhconditions.WithMessage("OGX module webhook state is unknown"),
		)
	}

	if healthSummary.UnhealthyCount > 0 {
		cm.MarkTrue(
			string(common.ConditionTypeDegraded),
			observed,
			odhconditions.WithReason("UnhealthyOGXServers"),
			odhconditions.WithMessage("Unhealthy OGXServer instances: %s", strings.Join(healthSummary.Unhealthy, ", ")),
			odhconditions.WithSeverity(common.ConditionSeverityInfo),
		)
	} else {
		cm.MarkFalse(
			string(common.ConditionTypeDegraded),
			observed,
			odhconditions.WithReason("Healthy"),
			odhconditions.WithMessage("No unhealthy OGXServer instances detected"),
			odhconditions.WithSeverity(common.ConditionSeverityInfo),
		)
	}

	cm.Sort()
	if cm.IsHappy() {
		instance.Status.Phase = common.PhaseReady
	} else {
		instance.Status.Phase = common.PhaseNotReady
	}

	return r.Status().Update(ctx, instance)
}

func (r *Reconciler) updateStatusOnError(
	ctx context.Context,
	instance *platformv1alpha1.OGX,
	provisioningSucceeded bool,
	rootOperatorReady bool,
	rootWebhookReady bool,
	reconcileErr error,
	statusLogMsg string,
) {
	if statusErr := r.updateStatus(ctx, instance, provisioningSucceeded, rootOperatorReady, rootWebhookReady, ogxServerHealthSummary{}, reconcileErr); statusErr != nil {
		log.FromContext(ctx).Error(statusErr, statusLogMsg)
	}
}

func (r *Reconciler) loadComponentReleases() (common.ComponentReleaseStatus, error) {
	path := filepath.Join(r.Config.ManifestsPath, componentName, "component_metadata.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return common.ComponentReleaseStatus{}, fmt.Errorf("failed to read component metadata %s: %w", path, err)
	}

	meta := componentMetadata{}
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return common.ComponentReleaseStatus{}, fmt.Errorf("failed to unmarshal component metadata %s: %w", path, err)
	}

	return common.ComponentReleaseStatus{Releases: meta.Releases}, nil
}

func managementState(instance *platformv1alpha1.OGX) common.ManagementState {
	if instance.Spec.ManagementState == common.Removed {
		return common.Removed
	}

	return common.Managed
}

func ensureFinalizer(instance *platformv1alpha1.OGX) bool {
	return controllerutil.AddFinalizer(instance, ogxFinalizer)
}

func clearFinalizer(instance *platformv1alpha1.OGX) bool {
	return controllerutil.RemoveFinalizer(instance, ogxFinalizer)
}

func (r *Reconciler) refreshPlatformVersion(ctx context.Context) error {
	version, err := r.readPlatformVersionFromConfigMap(ctx)
	if err != nil {
		return err
	}
	if version != "" && r.Config != nil {
		r.Config.PlatformVersion = version
	}

	return nil
}

func (r *Reconciler) readPlatformVersionFromConfigMap(ctx context.Context) (string, error) {
	if r.Config == nil || r.Config.ApplicationsNamespace == "" {
		return "", nil
	}

	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{
		Name:      platformConfigCMName,
		Namespace: r.Config.ApplicationsNamespace,
	}, cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}

		return "", fmt.Errorf("failed to get platform config ConfigMap %s/%s: %w", r.Config.ApplicationsNamespace, platformConfigCMName, err)
	}

	if cm.Data == nil {
		return "", nil
	}

	if version := strings.TrimSpace(cm.Data[platformVersionDataKey]); version != "" {
		return version, nil
	}

	return strings.TrimSpace(cm.Data[moduleconfig.KeyPlatformVersion]), nil
}

func (r *Reconciler) platformVersion() string {
	if r.Config == nil {
		return ""
	}

	return strings.TrimSpace(r.Config.PlatformVersion)
}

func (r *Reconciler) isPlatformConfigMap(obj client.Object) bool {
	if r.Config == nil {
		return false
	}

	return obj.GetName() == platformConfigCMName && obj.GetNamespace() == r.Config.ApplicationsNamespace
}

func setPlatformRelease(status *common.ComponentReleaseStatus, version string) {
	if status == nil || version == "" || version == moduleconfig.DefaultPlatformVersion {
		return
	}

	for i := range status.Releases {
		if status.Releases[i].Name == platformReleaseName {
			status.Releases[i].Version = version
			return
		}
	}

	status.Releases = append(status.Releases, common.ComponentRelease{
		Name:    platformReleaseName,
		Version: version,
	})
}

func overlayForPlatform(platformName string) string {
	value := strings.ToLower(strings.TrimSpace(platformName))
	if strings.Contains(value, "rhoai") || strings.Contains(value, "open shift ai") || strings.Contains(value, "openshift ai") {
		return overlayRhoai
	}

	return overlayODH
}

func hasImageParamOverrides() bool {
	for _, envNames := range imageParamEnvMap {
		for _, envName := range envNames {
			if os.Getenv(envName) != "" {
				return true
			}
		}
	}

	return false
}

func prepareManifestFSWithOverrides(componentRoot, overlay string) (filesys.FileSystem, string, error) {
	fs := filesys.MakeFsInMemory()
	renderRoot := componentName

	if err := copyDirToFS(fs, componentRoot, renderRoot); err != nil {
		return nil, "", err
	}

	paramsPath := pathpkg.Join(renderRoot, overlay, paramsFileName)
	if err := applyParamsEnvOverrides(fs, paramsPath); err != nil {
		return nil, "", err
	}

	return fs, pathpkg.Join(renderRoot, overlay), nil
}

func copyDirToFS(target filesys.FileSystem, srcRoot, dstRoot string) error {
	return filepath.Walk(srcRoot, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(srcRoot, current)
		if err != nil {
			return err
		}

		targetPath := dstRoot
		if rel != "." {
			targetPath = pathpkg.Join(dstRoot, filepath.ToSlash(rel))
		}

		if info.IsDir() {
			return target.MkdirAll(targetPath)
		}

		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}

		parent := pathpkg.Dir(targetPath)
		if parent != "." {
			if err := target.MkdirAll(parent); err != nil {
				return err
			}
		}

		return target.WriteFile(targetPath, data)
	})
}

func applyParamsEnvOverrides(target filesys.FileSystem, paramsPath string) error {
	if !target.Exists(paramsPath) {
		return nil
	}

	content, err := target.ReadFile(paramsPath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", paramsPath, err)
	}

	lines := strings.Split(string(content), "\n")
	changed := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])

		override, ok := resolveImageParamOverride(key)
		if !ok {
			continue
		}

		lines[i] = rewriteEnvLine(line, key, override)
		changed = true
	}

	if !changed {
		return nil
	}

	return target.WriteFile(paramsPath, []byte(strings.Join(lines, "\n")))
}

func resolveImageParamOverride(key string) (string, bool) {
	envNames, ok := imageParamEnvMap[key]
	if !ok {
		return "", false
	}

	for _, envName := range envNames {
		value := strings.TrimSpace(os.Getenv(envName))
		if value == "" {
			continue
		}

		if _, err := reference.ParseAnyReference(value); err != nil {
			log.Log.V(1).Info("skipping invalid image override", "env", envName, "error", err)
			continue
		}

		return value, true
	}

	return "", false
}

func rewriteEnvLine(line, key, value string) string {
	parts := strings.SplitN(line, "=", 2)
	if len(parts) < 2 {
		return key + "=" + value
	}

	rawValue := strings.TrimSpace(parts[1])
	if len(rawValue) >= 2 {
		if (rawValue[0] == '"' && rawValue[len(rawValue)-1] == '"') ||
			(rawValue[0] == '\'' && rawValue[len(rawValue)-1] == '\'') {
			return parts[0] + "=" + string(rawValue[0]) + value + string(rawValue[len(rawValue)-1])
		}
	}

	return parts[0] + "=" + value
}
