/*
Copyright 2019 The Crossplane Authors.

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
	"fmt"
	"time"

	"github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane/templating-controller/pkg/resource"
)

const (
	reconcileTimeout = 1 * time.Minute

	defaultShortWait = 30 * time.Second
	defaultLongWait  = 1 * time.Minute

	errUpdateResourceStatus  = "could not update status of the parent resource"
	errGetResource           = "could not get the parent resource"
	errTemplatingOperation   = "templating operation failed"
	errChildResourcePatchers = "child resource patchers failed"
	errApply                 = "apply failed"
	errCreateChildResource   = "could not create child resource"
	errGetChildResource      = "could not get child resource"
)

type TemplatingReconcilerOption func(*TemplatingReconciler)

func WithChildResourcePatcher(op ...resource.ChildResourcePatcher) TemplatingReconcilerOption {
	return func(reconciler *TemplatingReconciler) {
		reconciler.childResourcePatcher = op
	}
}

func WithTemplatingEngine(eng resource.TemplatingEngine) TemplatingReconcilerOption {
	return func(reconciler *TemplatingReconciler) {
		reconciler.templatingEngine = eng
	}
}

func WithShortWait(d time.Duration) TemplatingReconcilerOption {
	return func(reconciler *TemplatingReconciler) {
		reconciler.shortWait = d
	}
}

func WithLongWait(d time.Duration) TemplatingReconcilerOption {
	return func(reconciler *TemplatingReconciler) {
		reconciler.longWait = d
	}
}

func WithLogger(l logging.Logger) TemplatingReconcilerOption {
	return func(reconciler *TemplatingReconciler) {
		reconciler.log = l
	}
}

func NewTemplatingReconciler(m manager.Manager, of schema.GroupVersionKind, options ...TemplatingReconcilerOption) *TemplatingReconciler {
	nr := func() resource.ParentResource {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(of)
		return u
	}

	r := &TemplatingReconciler{
		kube:              m.GetClient(),
		newParentResource: nr,
		shortWait:         defaultShortWait,
		longWait:          defaultLongWait,
		log:               logging.NewNopLogger(),
		templatingEngine:  &resource.NopTemplatingEngine{},
		childResourcePatcher: resource.ChildResourcePatcherChain{
			resource.NewOwnerReferenceAdder(),
			resource.NewDefaultingAnnotationRemover(),
			resource.NewNamespacePatcher(),
			resource.NewLabelPropagator(),
			resource.NewParentLabelSetAdder(),
		},
	}

	for _, opt := range options {
		opt(r)
	}
	return r
}

type TemplatingReconciler struct {
	kube              client.Client
	newParentResource func() resource.ParentResource
	resourcePath      string
	shortWait         time.Duration
	longWait          time.Duration
	log               logging.Logger

	templatingEngine     resource.TemplatingEngine
	childResourcePatcher resource.ChildResourcePatcherChain
}

func (r *TemplatingReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()

	cr := r.newParentResource()
	if err := r.kube.Get(ctx, req.NamespacedName, cr); err != nil {
		// There's no need to requeue if the resource no longer exists. Otherwise
		// we'll be requeued implicitly because we return an error.
		return reconcile.Result{Requeue: false}, errors.Wrap(client.IgnoreNotFound(err), errGetResource)
	}

	if meta.WasDeleted(cr) {
		// We have nothing to do as the child resources will be garbage collected
		// by Kubernetes.
		return reconcile.Result{Requeue: false}, nil
	}

	childResources, err := r.templatingEngine.Run(cr)
	if err != nil {
		r.onlyLog(resource.SetConditions(cr, v1alpha1.ReconcileError(errors.Wrap(err, errTemplatingOperation))))
		return ctrl.Result{RequeueAfter: r.shortWait}, errors.Wrap(r.kube.Status().Update(ctx, cr), errUpdateResourceStatus)
	}

	childResources, err = r.childResourcePatcher.Patch(cr, childResources)
	if err != nil {
		r.onlyLog(resource.SetConditions(cr, v1alpha1.ReconcileError(errors.Wrap(err, errChildResourcePatchers))))
		return ctrl.Result{RequeueAfter: r.shortWait}, errors.Wrap(r.kube.Status().Update(ctx, cr), errUpdateResourceStatus)
	}

	for _, o := range childResources {
		if err := Apply(ctx, r.kube, o); err != nil {
			r.onlyLog(resource.SetConditions(cr, v1alpha1.ReconcileError(errors.Wrap(err, fmt.Sprintf("%s: %s/%s of type %s", errApply, o.GetName(), o.GetNamespace(), o.GetObjectKind().GroupVersionKind().String())))))
			return ctrl.Result{RequeueAfter: r.shortWait}, errors.Wrap(r.kube.Status().Update(ctx, cr), errUpdateResourceStatus)
		}
	}

	r.onlyLog(resource.SetConditions(cr, v1alpha1.ReconcileSuccess()))
	return ctrl.Result{RequeueAfter: r.longWait}, errors.Wrap(r.kube.Status().Update(ctx, cr), errUpdateResourceStatus)
}

// Apply creates if the object doesn't exist and patches if it does exists.
func Apply(ctx context.Context, kube client.Client, o resource.ChildResource) error {
	existing := o.DeepCopyObject().(resource.ChildResource)
	err := kube.Get(ctx, types.NamespacedName{Name: o.GetName(), Namespace: o.GetNamespace()}, existing)
	if kerrors.IsNotFound(err) {
		return errors.Wrap(kube.Create(ctx, o), errCreateChildResource)
	}
	if err != nil {
		return errors.Wrap(err, errGetChildResource)
	}
	patchJSON, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return kube.Patch(ctx, existing, client.ConstantPatch(types.MergePatchType, patchJSON))
}

func (r *TemplatingReconciler) onlyLog(err error) {
	if err != nil {
		r.log.Info(err.Error())
	}
}
