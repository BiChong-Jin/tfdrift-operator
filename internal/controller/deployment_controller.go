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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/<you>/tfdrift-operator/internal/drift"
)

type DeploymentReconciler struct {
	client.Client
	Log      logr.Logger
	Recorder record.EventRecorder
}

// RBAC (kubebuilder markers)
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("deployment", req.NamespacedName)

	var dep appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only enforce when enabled
	if dep.Labels == nil || dep.Labels[drift.LabelEnabled] != "true" {
		return ctrl.Result{}, nil
	}

	expected := ""
	if dep.Annotations != nil {
		expected = dep.Annotations[drift.AnnExpectedHash]
	}
	if expected == "" {
		// No baseline to compare; mark last checked and exit
		return r.patchAnnotations(ctx, &dep, map[string]string{
			drift.AnnLastCheckedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}

	liveHash, err := drift.HashDeployment(&dep)
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
		if dep.Annotations == nil || dep.Annotations[drift.AnnDriftedAt] == "" {
			patch[drift.AnnDriftedAt] = now
		}

		r.Recorder.Eventf(&dep, corev1.EventTypeWarning, "TerraformDriftDetected",
			"Deployment drift detected: expectedHash=%s liveHash=%s", expected, liveHash)
		log.Info("drift detected", "expected", expected, "live", liveHash)
	} else {
		patch[drift.AnnDrifted] = "false"
	}

	return r.patchAnnotations(ctx, &dep, patch)
}

func (r *DeploymentReconciler) patchAnnotations(ctx context.Context, dep *appsv1.Deployment, kv map[string]string) (ctrl.Result, error) {
	orig := dep.DeepCopy()

	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	for k, v := range kv {
		dep.Annotations[k] = v
	}

	if err := r.Patch(ctx, dep, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("tfdrift-operator")
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Complete(r)
}

