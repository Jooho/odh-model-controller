/*
Copyright 2025.

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

package llm

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/hashicorp/go-multierror"
	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	kuadrantv1 "github.com/kuadrant/kuadrant-operator/api/v1"
	"github.com/opendatahub-io/odh-model-controller/internal/controller/constants"
	"github.com/opendatahub-io/odh-model-controller/internal/controller/serving/llm/reconcilers"
	parentreconcilers "github.com/opendatahub-io/odh-model-controller/internal/controller/serving/reconcilers"
	"github.com/opendatahub-io/odh-model-controller/internal/controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type LLMInferenceServiceReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	subResourceReconcilers []parentreconcilers.LLMSubResourceReconciler
}

func isAuthPolicyCRDAvailable(config *rest.Config) (bool, error) {
	return utils.IsCrdAvailable(config, constants.GetAuthPolicyGroupVersion(), constants.AuthPolicyKind)
}

func NewLLMInferenceServiceReconciler(client client.Client, scheme *runtime.Scheme, config *rest.Config) *LLMInferenceServiceReconciler {

	var subResourceReconcilers []parentreconcilers.LLMSubResourceReconciler

	if isAuthPolicyAvailable, err := isAuthPolicyCRDAvailable(config); err == nil && isAuthPolicyAvailable {
		subResourceReconcilers = append(subResourceReconcilers, reconcilers.NewKserveAuthPolicyReconciler(client, scheme))
	}

	return &LLMInferenceServiceReconciler{
		Client:                 client,
		Scheme:                 scheme,
		subResourceReconcilers: subResourceReconcilers,
	}
}

func (r *LLMInferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("LLMInferenceService", req.Name, "namespace", req.Namespace)

	llmisvc := &kservev1alpha1.LLMInferenceService{}
	err := r.Client.Get(ctx, req.NamespacedName, llmisvc)
	if err != nil && apierrs.IsNotFound(err) {
		logger.Info("Stop LLMInferenceService reconciliation")
		return ctrl.Result{}, nil
	} else if err != nil {
		logger.Error(err, "Unable to fetch the LLMInferenceService")
		return ctrl.Result{}, err
	}

	if !llmisvc.GetDeletionTimestamp().IsZero() {
		logger.Info("LLMInferenceService being deleted, cleaning up sub-resources")
		if err := r.onDeletion(ctx, logger, llmisvc); err != nil {
			logger.Error(err, "Failed to cleanup sub-resources during LLMInferenceService deletion")
			return ctrl.Result{}, err
		}
		if err := r.DeleteResourcesIfNoLLMIsvcExists(ctx, logger, llmisvc.Namespace); err != nil {
			logger.Error(err, "Failed to cleanup namespace resources")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.reconcileSubResources(ctx, logger, llmisvc); err != nil {
		logger.Error(err, "Failed to reconcile LLMInferenceService sub-resources")
		return ctrl.Result{}, err
	}

	logger.V(1).Info("Successfully reconciled LLMInferenceService")
	return ctrl.Result{}, nil
}

// +kubebuilder:rbac:groups=serving.kserve.io,resources=llminferenceservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=serving.kserve.io,resources=llminferenceservices/finalizers,verbs=get;list;watch;update;create;patch;delete
// +kubebuilder:rbac:groups=kuadrant.io,resources=authpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *LLMInferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager, setupLog logr.Logger) error {
	logger := mgr.GetLogger().WithName("LLMInferenceService.SetupWithManager")

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&kservev1alpha1.LLMInferenceService{}).
		Named("llminferenceservice")

	setupLog.Info("Setting up LLMInferenceService controller")

	if ok, err := utils.IsCrdAvailable(mgr.GetConfig(), kuadrantv1.GroupVersion.String(), "AuthPolicy"); ok && err == nil {
		builder = builder.Watches(&kuadrantv1.AuthPolicy{},
			r.enqueueOnAuthPolicyChange(logger)).
			WithEventFilter(predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool {
					return false // Ignore create events
				},
				UpdateFunc: func(e event.UpdateEvent) bool {
					return true // Process update events only
				},
				DeleteFunc: func(e event.DeleteEvent) bool {
					return true // Process delete events only
				},
			})
	}

	return builder.Complete(r)
}

func makeReconcileRequest(llmSvc kservev1alpha1.LLMInferenceService) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{
		Namespace: llmSvc.Namespace,
		Name:      llmSvc.Name,
	}}
}

func (r *LLMInferenceServiceReconciler) enqueueOnAuthPolicyChange(logger logr.Logger) handler.EventHandler {
	logger = logger.WithName("enqueueOnAuthPolicyChange")
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
		sub := object.(*kuadrantv1.AuthPolicy)
		reqs := make([]reconcile.Request, 0, 2)
		targetKind := sub.Spec.TargetRef.Kind
		gatewayNamespace, gatewayName, err := utils.GetGatewayInfoFromConfigMap(ctx, r.Client)
		if err != nil {
			logger.Error(err, "Failed to get gateway info from config map")
			return reqs
		}

		listNamespace := corev1.NamespaceAll
		continueToken := ""
		for {
			llmSvcList := &kservev1alpha1.LLMInferenceServiceList{}
			if err := r.Client.List(ctx, llmSvcList, &client.ListOptions{Namespace: listNamespace, Continue: continueToken}); err != nil {
				logger.Error(err, "Failed to list LLMInferenceService")
				return reqs
			}
			for _, llmSvc := range llmSvcList.Items {
				if targetKind == "HTTPRoute" {
					if constants.GetHTTPRouteName(llmSvc.Name) != string(sub.Spec.TargetRef.Name) {
						continue
					} else {
						reqs = append(reqs, makeReconcileRequest(llmSvc))
						return reqs
					}
				} else if targetKind == "Gateway" {
					isAffected := false

					if llmSvc.Spec.Router.Gateway == nil {
						isAffected = sub.Namespace == gatewayNamespace && string(sub.Spec.TargetRef.Name) == gatewayName
					} else {
						// Path 2: Using explicit gateway references
						for _, ref := range llmSvc.Spec.Router.Gateway.Refs {
							if string(ref.Name) == sub.Name && string(ref.Namespace) == sub.Namespace {
								isAffected = true
								break
							}
						}
					}

					if isAffected {
						reqs = append(reqs, makeReconcileRequest(llmSvc))
						return reqs
					}
				}
			}

			if llmSvcList.Continue == "" {
				break
			}
			continueToken = llmSvcList.Continue
		}

		return reqs
	})
}

func (r *LLMInferenceServiceReconciler) onDeletion(ctx context.Context, log logr.Logger, llmisvc *kservev1alpha1.LLMInferenceService) error {
	log.V(1).Info("Triggering Delete for LLMInferenceService", "name", llmisvc.Name)
	var deleteErrors *multierror.Error

	for _, subReconciler := range r.subResourceReconcilers {
		if err := subReconciler.Delete(ctx, log, llmisvc); err != nil {
			log.Error(err, "Failed to delete sub-resource")
			deleteErrors = multierror.Append(deleteErrors, err)
		}
	}

	return deleteErrors.ErrorOrNil()
}

func (r *LLMInferenceServiceReconciler) reconcileSubResources(ctx context.Context, log logr.Logger, llmisvc *kservev1alpha1.LLMInferenceService) error {
	log.V(1).Info("Reconciling LLMInferenceService sub-resources")
	var reconcileErrors *multierror.Error

	for _, subReconciler := range r.subResourceReconcilers {
		if err := subReconciler.Reconcile(ctx, log, llmisvc); err != nil {
			log.Error(err, "Failed to reconcile sub-resource")
			reconcileErrors = multierror.Append(reconcileErrors, err)
		}
	}

	return reconcileErrors.ErrorOrNil()
}

func (r *LLMInferenceServiceReconciler) Cleanup(ctx context.Context, log logr.Logger, isvcNs string) error {
	log.V(1).Info("Cleaning up LLMInferenceService sub-resources", "namespace", isvcNs)
	var cleanupErrors *multierror.Error

	for _, subReconciler := range r.subResourceReconcilers {
		if err := subReconciler.Cleanup(ctx, log, isvcNs); err != nil {
			log.Error(err, "Failed to cleanup sub-resource")
			cleanupErrors = multierror.Append(cleanupErrors, err)
		}
	}

	return cleanupErrors.ErrorOrNil()
}

func (r *LLMInferenceServiceReconciler) DeleteResourcesIfNoLLMIsvcExists(ctx context.Context, log logr.Logger, namespace string) error {
	llmInferenceServiceList := &kservev1alpha1.LLMInferenceServiceList{}
	if err := r.Client.List(ctx, llmInferenceServiceList, client.InNamespace(namespace)); err != nil {
		return err
	}

	var existingLLMIsvcs []kservev1alpha1.LLMInferenceService
	for _, llmisvc := range llmInferenceServiceList.Items {
		if llmisvc.GetDeletionTimestamp() == nil {
			existingLLMIsvcs = append(existingLLMIsvcs, llmisvc)
		}
	}

	if len(existingLLMIsvcs) == 0 {
		log.V(1).Info("Triggering LLMInferenceService Cleanup for Namespace", "namespace", namespace)
		if err := r.Cleanup(ctx, log, namespace); err != nil {
			return err
		}
	}

	return nil
}
