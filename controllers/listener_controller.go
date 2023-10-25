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

package controllers

import (
	"context"

	"github.com/go-logr/logr"

	"google.golang.org/protobuf/encoding/protojson"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"

	v1alpha1 "github.com/kaasops/envoy-xds-controller/api/v1alpha1"
	"github.com/kaasops/envoy-xds-controller/pkg/config"
	"github.com/kaasops/envoy-xds-controller/pkg/errors"
	"github.com/kaasops/envoy-xds-controller/pkg/factory/virtualservice"
	"github.com/kaasops/envoy-xds-controller/pkg/factory/virtualservice/tls"
	"github.com/kaasops/envoy-xds-controller/pkg/options"
	"github.com/kaasops/envoy-xds-controller/pkg/utils/k8s"
	xdscache "github.com/kaasops/envoy-xds-controller/pkg/xds/cache"

	api_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ListenerReconciler reconciles a Listener object
type ListenerReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Cache           *xdscache.Cache
	Unmarshaler     protojson.UnmarshalOptions
	DiscoveryClient *discovery.DiscoveryClient
	Config          *config.Config

	log logr.Logger
}

//+kubebuilder:rbac:groups=envoy.kaasops.io,resources=listeners,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=envoy.kaasops.io,resources=listeners/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=envoy.kaasops.io,resources=listeners/finalizers,verbs=update

func (r *ListenerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.log = log.FromContext(ctx).WithValues("Envoy Listener", req.NamespacedName)
	r.log.Info("Reconciling listener")

	// Get listener instance
	instance := &v1alpha1.Listener{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if api_errors.IsNotFound(err) {
			r.log.V(1).Info("Listener instance not found. Delete object from xDS cache")
			nodeIDs, err := r.Cache.GetNodeIDsForResource(resourcev3.ListenerType, getResourceName(req.Namespace, req.Name))
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, errors.GetNodeIDForResource)
			}
			for _, nodeID := range nodeIDs {
				if err := r.Cache.Delete(nodeID, resourcev3.ListenerType, getResourceName(req.Namespace, req.Name)); err != nil {
					return ctrl.Result{}, errors.Wrap(err, errors.CannotDeleteFromCacheMessage)
				}
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrap(err, errors.GetFromKubernetesMessage)
	}

	if instance.Spec == nil {
		return ctrl.Result{}, errors.New(errors.EmptySpecMessage)
	}

	// Get Envoy Listener from listener instance spec
	listener := &listenerv3.Listener{}
	if err := r.Unmarshaler.Unmarshal(instance.Spec.Raw, listener); err != nil {
		return ctrl.Result{}, errors.Wrap(err, errors.UnmarshalMessage)
	}

	// Get VirtualService objects with matching listener
	virtualServices := &v1alpha1.VirtualServiceList{}
	listOpts := []client.ListOption{
		client.InNamespace(req.Namespace),
		client.MatchingFields{options.VirtualServiceListenerFeild: req.Name},
	}
	if err = r.List(ctx, virtualServices, listOpts...); err != nil {
		return ctrl.Result{}, errors.Wrap(err, errors.GetFromKubernetesMessage)
	}

	listener.Name = getResourceName(req.Namespace, req.Name)

	var chains []*listenerv3.FilterChain
	var routeConfigs []*routev3.RouteConfiguration
	var errs []error
	index, err := k8s.IndexCertificateSecrets(ctx, r.Client, instance.Namespace)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "cannot generate TLS certificates index from Kubernetes secrets")
	}

	for _, vs := range virtualServices.Items {
		tlsFactory := tls.NewTlsFactory(ctx, vs.Spec.TlsConfig, r.Client, r.DiscoveryClient, r.Config.GetDefaultIssuer(), instance.Namespace, index)
		vsFactory := virtualservice.NewVirtualServiceFactory(r.Client, r.Unmarshaler, &vs, instance, *tlsFactory)

		virtSvc, err := vsFactory.Create(ctx, getResourceName(vs.Namespace, vs.Name))
		if err != nil {
			if errors.NeedStatusUpdate(err) {
				if err := vs.SetError(ctx, r.Client, errors.Wrap(err, "cannot get Virtual Service struct").Error()); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			errs = append(errs, err)
			continue
		}

		if len(virtSvc.Tls.ErrorDomains) > 0 {
			if err := vs.SetDomainsStatus(ctx, r.Client, virtSvc.Tls.ErrorDomains); err != nil {
				errs = append(errs, err)
			}
		}

		// If VirtualService nodeIDs is not empty and listener does not contains all of them - skip. TODO: Add to status
		if !k8s.NodeIDsContains(virtSvc.NodeIDs, k8s.NodeIDs(instance)) {
			r.log.Info("NodeID mismatch", "VirtualService", vs.Name)
			if err := vs.SetError(ctx, r.Client, "VirtualService nodeIDs is not empty and listener does not contains all of them"); err != nil {
				errs = append(errs, err)
			}
			continue
		}

		routeConfigs = append(routeConfigs, virtSvc.RouteConfig)

		ch, err := virtualservice.FilterChains(&virtSvc)
		if err != nil {
			if errors.NeedStatusUpdate(err) {
				if err := vs.SetError(ctx, r.Client, errors.Wrap(err, "failed to get filterchain").Error()); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}

		chains = append(chains, ch...)

		if err := vs.SetValid(ctx, r.Client); err != nil {
			errs = append(errs, err)
		}

		if err := vs.SetLastAppliedHash(ctx, r.Client); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) != 0 {
		for _, e := range errs {
			r.log.Error(e, "Can't create FilterChain for listener")
		}
		return ctrl.Result{}, errors.New("failed to generate FilterChains or RouteConfigs")
	}

	listener.FilterChains = append(listener.FilterChains, chains...)

	// Add routeConfigs to xds cache
	for _, rtConfig := range routeConfigs {
		for _, nodeID := range k8s.NodeIDs(instance) {
			r.log.V(1).Info("Adding route", "name:", rtConfig.Name)
			if err := r.Cache.Update(nodeID, rtConfig); err != nil {
				return ctrl.Result{}, errors.Wrap(err, errors.CannotUpdateCacheMessage)
			}
		}
	}

	if err := listener.ValidateAll(); err != nil {
		return reconcile.Result{}, errors.WrapUKS(err, errors.CannotValidateCacheResourceMessage)
	}

	// Add listener to xds cache
	for _, nodeID := range k8s.NodeIDs(instance) {
		if len(listener.FilterChains) == 0 {
			r.log.WithValues("NodeID", nodeID).Info("Listener FilterChain is empty, deleting")
			if err := r.Cache.Delete(nodeID, resourcev3.ListenerType, getResourceName(req.Namespace, req.Name)); err != nil {
				return ctrl.Result{}, errors.Wrap(err, errors.CannotDeleteFromCacheMessage)
			}
			return ctrl.Result{}, nil
		}

		if err := r.Cache.Update(nodeID, listener); err != nil {
			return ctrl.Result{}, errors.Wrap(err, errors.CannotUpdateCacheMessage)
		}
	}

	r.log.Info("Listener reconcilation finished")

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ListenerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Add listener name to index
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.VirtualService{}, options.VirtualServiceListenerFeild, func(rawObject client.Object) []string {
		virtualService := rawObject.(*v1alpha1.VirtualService)
		// if listener feild is empty use default listener name as index
		if virtualService.Spec.Listener == nil {
			return []string{options.DefaultListenerName}
		}
		return []string{virtualService.Spec.Listener.Name}
	}); err != nil {
		return errors.Wrap(err, "cannot add Listener names to Listener Reconcile Index")
	}

	// EnqueueRequestsFromMapFunc
	// List all VirtualServies and finds listener ref
	listenerRequestMapper := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		var virtualServiceList v1alpha1.VirtualServiceList
		var reconcileRequests []reconcile.Request
		uniq := make(map[v1alpha1.ResourceRef]struct{})
		if err := mgr.GetCache().List(ctx, &virtualServiceList); err != nil {
			r.log.Error(err, "failed to list VirtualService resources")
			return nil
		}
		for _, vs := range virtualServiceList.Items {

			if refContains(virtualServiceResourceRefMapper(obj, vs), obj) {
				name := vs.Spec.Listener.Name
				namespace := obj.GetNamespace()
				resourceRef := v1alpha1.ResourceRef{Name: name}
				_, ok := uniq[resourceRef]
				if ok {
					continue
				}
				reconcileRequests = append(reconcileRequests, reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      name,
					Namespace: namespace,
				}})
				uniq[resourceRef] = struct{}{}
			}
		}
		return reconcileRequests
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Listener{}).
		Watches(&v1alpha1.VirtualService{}, &virtualservice.EnqueueRequestForVirtualService{}).
		Watches(&v1alpha1.AccessLogConfig{}, listenerRequestMapper).
		Watches(&v1alpha1.Route{}, listenerRequestMapper).
		Complete(r)
}
