/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package redis

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	redisv1 "github.com/openstack-k8s-operators/infra-operator/apis/redis/v1beta1"
	"github.com/openstack-k8s-operators/lib-common/modules/common"
	"github.com/openstack-k8s-operators/lib-common/modules/common/configmap"
	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"github.com/openstack-k8s-operators/lib-common/modules/common/tls"
	"github.com/openstack-k8s-operators/lib-common/modules/common/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"

	redis "github.com/openstack-k8s-operators/infra-operator/pkg/redis"
	condition "github.com/openstack-k8s-operators/lib-common/modules/common/condition"

	common_rbac "github.com/openstack-k8s-operators/lib-common/modules/common/rbac"
	commonservice "github.com/openstack-k8s-operators/lib-common/modules/common/service"
	commonstatefulset "github.com/openstack-k8s-operators/lib-common/modules/common/statefulset"
)

// GetLogger returns a logger object with a prefix of "controller.name" and additional controller context fields
func (r *Reconciler) GetLogger(ctx context.Context) logr.Logger {
	return log.FromContext(ctx).WithName("Controllers").WithName("Redis")
}

// fields to index to reconcile on CR change
const (
	serviceSecretNameField = ".spec.tls.genericService.SecretName"
	caSecretNameField      = ".spec.tls.ca.caBundleSecretName"
)

var (
	allWatchFields = []string{
		serviceSecretNameField,
		caSecretNameField,
	}
)

// Reconciler reconciles a Redis object
type Reconciler struct {
	client.Client
	Kclient kubernetes.Interface
	config  *rest.Config
	Scheme  *runtime.Scheme
}

// RBAC for redis resources
//+kubebuilder:rbac:groups=redis.openstack.org,resources=redises,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=redis.openstack.org,resources=redises/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=redis.openstack.org,resources=redises/finalizers,verbs=update

// RBAC for deployments and their pods
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;

// RBAC for services
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete;

// service account, role, rolebinding
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=roles,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;list;watch;create;update
// service account permissions that are needed to grant permission to the above
// +kubebuilder:rbac:groups="security.openshift.io",resourceNames=anyuid,resources=securitycontextconstraints,verbs=use
// +kubebuilder:rbac:groups="",resources=pods,verbs=create;delete;get;list;patch;update;watch

// Reconcile - Redis
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, _err error) {
	Log := r.GetLogger(ctx)

	// Fetch the Redis instance
	instance := &redisv1.Redis{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	helper, err := helper.NewHelper(
		instance,
		r.Client,
		r.Kclient,
		r.Scheme,
		Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always patch the instance status when exiting this function so we can persist any changes.
	defer func() {
		// update the Ready condition based on the sub conditions
		if instance.Status.Conditions.AllSubConditionIsTrue() {
			instance.Status.Conditions.MarkTrue(
				condition.ReadyCondition, condition.ReadyMessage)
		} else {
			// something is not ready so reset the Ready condition
			instance.Status.Conditions.MarkUnknown(
				condition.ReadyCondition, condition.InitReason, condition.ReadyInitMessage)
			// and recalculate it based on the state of the rest of the conditions
			instance.Status.Conditions.Set(
				instance.Status.Conditions.Mirror(condition.ReadyCondition))
		}
		err := helper.PatchInstance(ctx, instance)
		if err != nil {
			_err = err
			return
		}
	}()

	//
	// initialize status
	//
	if instance.Status.Conditions == nil {
		instance.Status.Conditions = condition.Conditions{}
		// initialize conditions used later as Status=Unknown
		cl := condition.CreateList(
			// TLS cert secrets
			condition.UnknownCondition(condition.TLSInputReadyCondition, condition.InitReason, condition.InputReadyInitMessage),
			// endpoint for adoption redirect
			condition.UnknownCondition(condition.ExposeServiceReadyCondition, condition.InitReason, condition.ExposeServiceReadyInitMessage),
			// configmap generation
			condition.UnknownCondition(condition.ServiceConfigReadyCondition, condition.InitReason, condition.ServiceConfigReadyInitMessage),
			// redis pods ready
			condition.UnknownCondition(condition.DeploymentReadyCondition, condition.InitReason, condition.DeploymentReadyInitMessage),
			// service account, role, rolebinding conditions
			condition.UnknownCondition(condition.ServiceAccountReadyCondition, condition.InitReason, condition.ServiceAccountReadyInitMessage),
			condition.UnknownCondition(condition.RoleReadyCondition, condition.InitReason, condition.RoleReadyInitMessage),
			condition.UnknownCondition(condition.RoleBindingReadyCondition, condition.InitReason, condition.RoleBindingReadyInitMessage),
		)

		instance.Status.Conditions.Init(&cl)

		// Register overall status immediately to have an early feedback e.g. in the cli
		return ctrl.Result{}, nil
	}

	//
	// Create/Update all the resources associated to this Redis instance
	//

	// Service account, role, binding
	rbacRules := []rbacv1.PolicyRule{
		{
			APIGroups:     []string{"security.openshift.io"},
			ResourceNames: []string{"anyuid"},
			Resources:     []string{"securitycontextconstraints"},
			Verbs:         []string{"use"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"create", "get", "list", "watch", "update", "patch", "delete"},
		},
	}
	rbacResult, err := common_rbac.ReconcileRbac(ctx, helper, instance, rbacRules)
	if err != nil {
		return rbacResult, err
	} else if (rbacResult != ctrl.Result{}) {
		return rbacResult, nil
	}

	// Hash of all resources that may cause a service restart
	inputHashEnv := make(map[string]env.Setter)

	// Check and hash inputs
	var certHash, caHash string
	specTLS := &instance.Spec.TLS
	if specTLS.Enabled() {
		certHash, _, err = specTLS.GenericService.ValidateCertSecret(ctx, helper, instance.Namespace)
		inputHashEnv["Cert"] = env.SetValue(certHash)
	}
	if err == nil && specTLS.Ca.CaBundleSecretName != "" {
		caName := types.NamespacedName{
			Name:      specTLS.Ca.CaBundleSecretName,
			Namespace: instance.Namespace,
		}
		caHash, _, err = tls.ValidateCACertSecret(ctx, helper.GetClient(), caName)
		inputHashEnv["CA"] = env.SetValue(caHash)
	}
	if err != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			condition.TLSInputReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.TLSInputErrorMessage,
			err.Error()))
		return ctrl.Result{}, fmt.Errorf("error calculating input hash: %w", err)
	}
	instance.Status.Conditions.MarkTrue(condition.TLSInputReadyCondition, condition.InputReadyMessage)

	// Redis config maps
	err = r.generateConfigMaps(ctx, helper, instance, &inputHashEnv)
	if err != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			condition.ServiceConfigReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.ServiceConfigReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, fmt.Errorf("error calculating configmap hash: %w", err)
	}
	instance.Status.Conditions.MarkTrue(condition.ServiceConfigReadyCondition, condition.ServiceConfigReadyMessage)

	//
	// create hash over all the different input resources to identify if any those changed
	// and a restart/recreate is required.
	//
	hashOfHashes, err := util.HashOfInputHashes(inputHashEnv)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hashMap, changed := util.SetHash(instance.Status.Hash, common.InputHashName, hashOfHashes); changed {
		// Hash changed and instance status should be updated (which will be done by main defer func),
		// so update all the input hashes and return to reconcile again
		instance.Status.Hash = hashMap
		for k, s := range inputHashEnv {
			var envVar corev1.EnvVar
			s(&envVar)
			instance.Status.Hash[k] = envVar.Value
		}
		util.LogForObject(helper, fmt.Sprintf("Input hash changed %s", hashOfHashes), instance)
		return ctrl.Result{}, nil
	}

	// the headless service provides DNS entries for pods
	// the name of the resource must match the name of the app selector
	headless, err := commonservice.NewService(redis.HeadlessService(instance), time.Duration(5)*time.Second, nil)
	if err != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			condition.ExposeServiceReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.ExposeServiceReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	hlres, hlerr := headless.CreateOrPatch(ctx, helper)
	if hlerr != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			condition.ExposeServiceReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.ExposeServiceReadyErrorMessage,
			err.Error()))
		return hlres, hlerr
	}

	// Service to expose Redis pods
	commonsvc, err := commonservice.NewService(redis.Service(instance), time.Duration(5)*time.Second, nil)
	if err != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			condition.ExposeServiceReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.ExposeServiceReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	sres, serr := commonsvc.CreateOrPatch(ctx, helper)
	if serr != nil {
		instance.Status.Conditions.Set(condition.FalseCondition(
			condition.ExposeServiceReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.ExposeServiceReadyErrorMessage,
			err.Error()))
		return sres, serr
	}
	instance.Status.Conditions.MarkTrue(condition.ExposeServiceReadyCondition, condition.ExposeServiceReadyMessage)

	//
	// Reconstruct the state of the redis resource based on the deployment and its pods
	//

	// Statefulset
	commonstatefulset := commonstatefulset.NewStatefulSet(redis.StatefulSet(instance), 5)
	sfres, sferr := commonstatefulset.CreateOrPatch(ctx, helper)
	if sferr != nil {
		return sfres, sferr
	}
	statefulset := commonstatefulset.GetStatefulSet()

	if statefulset.Status.ReadyReplicas > 0 {
		instance.Status.Conditions.MarkTrue(condition.DeploymentReadyCondition, condition.DeploymentReadyMessage)
	}

	return ctrl.Result{}, nil
}

// generateConfigMaps returns the config map resource for a redis instance
func (r *Reconciler) generateConfigMaps(
	ctx context.Context,
	h *helper.Helper,
	instance *redisv1.Redis,
	envVars *map[string]env.Setter,
) error {
	templateParameters := make(map[string]interface{})
	customData := make(map[string]string)

	cms := []util.Template{
		// ScriptsConfigMap
		{
			Name:         fmt.Sprintf("%s-scripts", instance.Name),
			Namespace:    instance.Namespace,
			Type:         util.TemplateTypeScripts,
			InstanceType: instance.Kind,
			Labels:       map[string]string{},
		},
		// ConfigMap
		{
			Name:          fmt.Sprintf("%s-config-data", instance.Name),
			Namespace:     instance.Namespace,
			Type:          util.TemplateTypeConfig,
			InstanceType:  instance.Kind,
			CustomData:    customData,
			ConfigOptions: templateParameters,
			Labels:        map[string]string{},
		},
	}

	err := configmap.EnsureConfigMaps(ctx, h, instance, cms, envVars)
	if err != nil {
		util.LogErrorForObject(h, err, "Unable to retrieve or create config maps", instance)
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.config = mgr.GetConfig()

	// Various CR fields need to be indexed to filter watch events
	// for the secret changes we want to be notified of
	// index caBundleSecretName
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &redisv1.Redis{}, caSecretNameField, func(rawObj client.Object) []string {
		// Extract the secret name from the spec, if one is provided
		cr := rawObj.(*redisv1.Redis)
		tls := &cr.Spec.TLS
		if tls.Ca.CaBundleSecretName != "" {
			return []string{tls.Ca.CaBundleSecretName}
		}
		return nil
	}); err != nil {
		return err
	}
	// index secretName
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &redisv1.Redis{}, serviceSecretNameField, func(rawObj client.Object) []string {
		// Extract the secret name from the spec, if one is provided
		cr := rawObj.(*redisv1.Redis)
		tls := &cr.Spec.TLS
		if tls.Enabled() {
			return []string{*tls.GenericService.SecretName}
		}
		return nil
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1.Redis{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Complete(r)
}

// findObjectsForSrc - returns a reconcile request if the object is referenced by a Redis CR
func (r *Reconciler) findObjectsForSrc(src client.Object) []reconcile.Request {
	requests := []reconcile.Request{}

	for _, field := range allWatchFields {
		crList := &redisv1.RedisList{}
		listOps := &client.ListOptions{
			FieldSelector: fields.OneTermEqualSelector(field, src.GetName()),
			Namespace:     src.GetNamespace(),
		}
		err := r.List(context.TODO(), crList, listOps)
		if err != nil {
			return []reconcile.Request{}
		}

		for _, item := range crList.Items {
			requests = append(requests,
				reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      item.GetName(),
						Namespace: item.GetNamespace(),
					},
				},
			)
		}
	}

	return requests
}
