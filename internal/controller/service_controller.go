/*
Copyright 2026.

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

package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Bichong-Jin/tfdrift-operator/internal/drift"
)

type ServiceReconciler struct {
	client.Client
	Log      logr.Logger
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("service", req.NamespacedName)

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if svc.Labels == nil || svc.Labels[drift.LabelEnabled] != "true" {
		return ctrl.Result{}, nil
	}

	expected := ""
	if svc.Annotations != nil {
		expected = svc.Annotations[drift.AnnExpectedHash]
	}
	if expected == "" {
		return r.patchAnnotations(ctx, &svc, map[string]string{
			drift.AnnLastCheckedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}

	liveHash, err := drift.HashService(&svc)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]string{
		drift.AnnLiveHash:      liveHash,
		drift.AnnLastCheckedAt: now,
	}

	drifted := (liveHash != expected)
	if drifted {
		patch[drift.AnnDrifted] = "true"
		if svc.Annotations == nil || svc.Annotations[drift.AnnDriftedAt] == "" {
			patch[drift.AnnDriftedAt] = now
		}

		r.Recorder.Eventf(&svc, corev1.EventTypeWarning, "TerraformDriftDetected",
			"Service drift detected: expectedHash=%s liveHash=%s", expected, liveHash)
		log.Info("drift detected", "expected", expected, "live", liveHash)
	} else {
		patch[drift.AnnDrifted] = "false"
	}

	return r.patchAnnotations(ctx, &svc, patch)
}

func (r *ServiceReconciler) patchAnnotations(ctx context.Context, svc *corev1.Service, kv map[string]string) (ctrl.Result, error) {
	orig := svc.DeepCopy()

	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	for k, v := range kv {
		svc.Annotations[k] = v
	}

	if err := r.Patch(ctx, svc, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("tfdrift-operator")
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}



