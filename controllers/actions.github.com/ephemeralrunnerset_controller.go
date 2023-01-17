/*
Copyright 2020 The actions-runner-controller authors.

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

package actionsgithubcom

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/actions/actions-runner-controller/apis/actions.github.com/v1alpha1"
	"github.com/actions/actions-runner-controller/github/actions"
	"github.com/go-logr/logr"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	ephemeralRunnerSetReconcilerOwnerKey = ".metadata.controller"
	ephemeralRunnerSetFinalizerName      = "ephemeralrunner.actions.github.com/finalizer"
)

// EphemeralRunnerSetReconciler reconciles a EphemeralRunnerSet object
type EphemeralRunnerSetReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	ActionsClient actions.MultiClient

	resourceBuilder resourceBuilder
}

//+kubebuilder:rbac:groups=actions.github.com,resources=ephemeralrunnersets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=actions.github.com,resources=ephemeralrunnersets/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// The responsibility of this controller is to bring the state to the desired one, but it should
// avoid patching itself, because of the frequent patches that the listener is doing.
// The safe point where we can patch the resource is when we are reacting on finalizer.
// Then, the listener should be deleted first, to allow controller clean up resources without interruptions
//
// The resource should be created with finalizer. To leave it to this controller to add it, we would
// risk the same issue of patching the status. Responsibility of this controller should only
// be to bring the count of EphemeralRunners to the desired one, not to patch this resource
// until it is safe to do so
func (r *EphemeralRunnerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ephemeralrunnerset", req.NamespacedName)

	ephemeralRunnerSet := new(v1alpha1.EphemeralRunnerSet)
	if err := r.Get(ctx, req.NamespacedName, ephemeralRunnerSet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Requested deletion does not need reconciled.
	if !ephemeralRunnerSet.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(ephemeralRunnerSet, ephemeralRunnerSetFinalizerName) {
			log.Info("Deleting resources")
			done, err := r.cleanUpEphemeralRunners(ctx, ephemeralRunnerSet, log)
			if err != nil {
				log.Error(err, "Failed to clean up EphemeralRunners")
				return ctrl.Result{}, err
			}
			if !done {
				log.Info("Waiting for resources to be deleted")
				return ctrl.Result{}, nil
			}

			log.Info("Removing finalizer")
			if err := patch(ctx, r.Client, ephemeralRunnerSet, func(obj *v1alpha1.EphemeralRunnerSet) {
				controllerutil.RemoveFinalizer(obj, ephemeralRunnerSetFinalizerName)
			}); err != nil && !kerrors.IsNotFound(err) {
				log.Error(err, "Failed to update ephemeral runner set with removed finalizer")
				return ctrl.Result{}, err
			}

			log.Info("Successfully removed finalizer after cleanup")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(ephemeralRunnerSet, ephemeralRunnerSetFinalizerName) {
		log.Info("Adding finalizer")
		if err := patch(ctx, r.Client, ephemeralRunnerSet, func(obj *v1alpha1.EphemeralRunnerSet) {
			controllerutil.AddFinalizer(obj, ephemeralRunnerSetFinalizerName)
		}); err != nil {
			log.Error(err, "Failed to update ephemeral runner set with finalizer added")
			return ctrl.Result{}, err
		}

		log.Info("Successfully added finalizer")
		return ctrl.Result{}, nil
	}

	// Find all EphemeralRunner with matching namespace and own by this EphemeralRunnerSet.
	ephemeralRunnerList := new(v1alpha1.EphemeralRunnerList)
	err := r.List(
		ctx,
		ephemeralRunnerList,
		client.InNamespace(req.Namespace),
		client.MatchingFields{ephemeralRunnerSetReconcilerOwnerKey: req.Name},
	)
	if err != nil {
		log.Error(err, "Unable to list child ephemeral runners")
		return ctrl.Result{}, err
	}

	pendingEphemeralRunners, runningEphemeralRunners, finishedEphemeralRunners, failedEphemeralRunners, deletingEphemeralRunners := categorizeEphemeralRunners(ephemeralRunnerList)

	log.Info("Ephemeral runner counts",
		"pending", len(pendingEphemeralRunners),
		"running", len(runningEphemeralRunners),
		"finished", len(finishedEphemeralRunners),
		"failed", len(failedEphemeralRunners),
		"deleting", len(deletingEphemeralRunners),
	)

	// cleanup finished runners and proceed
	var errs []error
	for i := range finishedEphemeralRunners {
		log.Info("Deleting finished ephemeral runner", "name", finishedEphemeralRunners[i].Name)
		if err := r.Delete(ctx, finishedEphemeralRunners[i]); err != nil {
			if !kerrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		mergedErrs := multierr.Combine(errs...)
		log.Error(mergedErrs, "Failed to delete finished ephemeral runners")
		return ctrl.Result{}, mergedErrs
	}

	total := len(pendingEphemeralRunners) + len(runningEphemeralRunners) + len(failedEphemeralRunners)
	log.Info("Scaling comparison", "current", total, "desired", ephemeralRunnerSet.Spec.Replicas)
	switch {
	case total < ephemeralRunnerSet.Spec.Replicas: // Handle scale up
		count := ephemeralRunnerSet.Spec.Replicas - total
		log.Info("Creating new ephemeral runners (scale up)", "count", count)
		if err := r.createEphemeralRunners(ctx, ephemeralRunnerSet, count, log); err != nil {
			log.Error(err, "failed to make ephemeral runner")
			return ctrl.Result{}, err
		}

	case total > ephemeralRunnerSet.Spec.Replicas: // Handle scale down scenario.
		count := total - ephemeralRunnerSet.Spec.Replicas
		log.Info("Deleting ephemeral runners (scale down)", "count", count)
		if err := r.deleteIdleEphemeralRunners(ctx, ephemeralRunnerSet, pendingEphemeralRunners, runningEphemeralRunners, count, log); err != nil {
			log.Error(err, "failed to delete idle runners")
			return ctrl.Result{}, err
		}
	}

	// Update the status if needed.
	if ephemeralRunnerSet.Status.CurrentReplicas != total {
		log.Info("Updating status with current runners count", "count", total)
		if err := patch(ctx, r.Status(), ephemeralRunnerSet, func(obj *v1alpha1.EphemeralRunnerSet) {
			obj.Status.CurrentReplicas = total
		}); err != nil {
			log.Error(err, "Failed to update status with current runners count")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *EphemeralRunnerSetReconciler) cleanUpEphemeralRunners(ctx context.Context, ephemeralRunnerSet *v1alpha1.EphemeralRunnerSet, log logr.Logger) (done bool, err error) {
	ephemeralRunnerList := new(v1alpha1.EphemeralRunnerList)
	err = r.List(ctx, ephemeralRunnerList, client.InNamespace(ephemeralRunnerSet.Namespace), client.MatchingFields{ephemeralRunnerSetReconcilerOwnerKey: ephemeralRunnerSet.Name})
	if err != nil {
		return false, fmt.Errorf("failed to list child ephemeral runners: %v", err)
	}

	// only if there are no ephemeral runners left, return true
	if len(ephemeralRunnerList.Items) == 0 {
		log.Info("All ephemeral runners are deleted")
		return true, nil
	}

	pendingEphemeralRunners, runningEphemeralRunners, finishedEphemeralRunners, failedEphemeralRunners, deletingEphemeralRunners := categorizeEphemeralRunners(ephemeralRunnerList)

	log.Info("Clean up runner counts",
		"pending", len(pendingEphemeralRunners),
		"running", len(runningEphemeralRunners),
		"finished", len(finishedEphemeralRunners),
		"failed", len(failedEphemeralRunners),
		"deleting", len(deletingEphemeralRunners),
	)

	log.Info("Cleanup finished or failed ephemeral runners")
	var errs []error
	for _, ephemeralRunner := range append(finishedEphemeralRunners, failedEphemeralRunners...) {
		log.Info("Deleting ephemeral runner", "name", ephemeralRunner.Name)
		if err := r.Delete(ctx, ephemeralRunner); err != nil && !kerrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		mergedErrs := multierr.Combine(errs...)
		log.Error(mergedErrs, "Failed to delete ephemeral runners")
		return false, mergedErrs
	}

	// avoid fetching the client if we have nothing left to do
	if len(runningEphemeralRunners) == 0 && len(pendingEphemeralRunners) == 0 {
		return false, nil
	}

	actionsClient, err := r.actionsClientFor(ctx, ephemeralRunnerSet)
	if err != nil {
		return false, err
	}

	log.Info("Cleanup pending or running ephemeral runners")
	errs = errs[0:0]
	for _, ephemeralRunner := range append(pendingEphemeralRunners, runningEphemeralRunners...) {
		log.Info("Removing the ephemeral runner from the service", "name", ephemeralRunner.Name)
		_, err := r.deleteEphemeralRunnerWithActionsClient(ctx, ephemeralRunner, actionsClient, log)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		mergedErrs := multierr.Combine(errs...)
		log.Error(mergedErrs, "Failed to remove ephemeral runners from the service")
		return false, mergedErrs
	}

	return false, nil
}

// createEphemeralRunners provisions `count` number of v1alpha1.EphemeralRunner resources in the cluster.
func (r *EphemeralRunnerSetReconciler) createEphemeralRunners(ctx context.Context, runnerSet *v1alpha1.EphemeralRunnerSet, count int, log logr.Logger) error {
	// Track multiple errors at once and return the bundle.
	errs := make([]error, 0)
	for i := 0; i < count; i++ {
		ephemeralRunner := r.resourceBuilder.newEphemeralRunner(runnerSet)

		// Make sure that we own the resource we create.
		if err := ctrl.SetControllerReference(runnerSet, ephemeralRunner, r.Scheme); err != nil {
			log.Error(err, "failed to set controller reference on ephemeral runner")
			errs = append(errs, err)
			continue
		}

		log.Info("Creating new ephemeral runner", "progress", i+1, "total", count)
		if err := r.Create(ctx, ephemeralRunner); err != nil {
			log.Error(err, "failed to make ephemeral runner")
			errs = append(errs, err)
			continue
		}

		log.Info("Created new ephemeral runner", "runner", ephemeralRunner.Name)
	}

	return multierr.Combine(errs...)
}

// deleteIdleEphemeralRunners try to deletes `count` number of v1alpha1.EphemeralRunner resources in the cluster.
// It will only delete `v1alpha1.EphemeralRunner` that has registered with Actions service
// which has a `v1alpha1.EphemeralRunner.Status.RunnerId` set.
// So, it is possible that this function will not delete enough ephemeral runners
// if there are not enough ephemeral runners that have registered with Actions service.
// When this happens, the next reconcile loop will try to delete the remaining ephemeral runners
// after we get notified by any of the `v1alpha1.EphemeralRunner.Status` updates.
func (r *EphemeralRunnerSetReconciler) deleteIdleEphemeralRunners(ctx context.Context, ephemeralRunnerSet *v1alpha1.EphemeralRunnerSet, pendingEphemeralRunners, runningEphemeralRunners []*v1alpha1.EphemeralRunner, count int, log logr.Logger) error {
	runners := newEphemeralRunnerStepper(pendingEphemeralRunners, runningEphemeralRunners)
	if runners.len() == 0 {
		log.Info("No pending or running ephemeral runners running at this time for scale down")
		return nil
	}
	actionsClient, err := r.actionsClientFor(ctx, ephemeralRunnerSet)
	if err != nil {
		return fmt.Errorf("failed to create actions client for ephemeral runner replica set: %v", err)
	}
	var errs []error
	deletedCount := 0
	for runners.next() {
		ephemeralRunner := runners.object()
		if ephemeralRunner.Status.RunnerId == 0 {
			log.Info("Skipping ephemeral runner since it is not registered yet", "name", ephemeralRunner.Name)
			continue
		}

		if ephemeralRunner.Status.JobRequestId > 0 {
			log.Info("Skipping ephemeral runner since it is running a job", "name", ephemeralRunner.Name, "jobRequestId", ephemeralRunner.Status.JobRequestId)
			continue
		}

		log.Info("Removing the idle ephemeral runner", "name", ephemeralRunner.Name)
		ok, err := r.deleteEphemeralRunnerWithActionsClient(ctx, ephemeralRunner, actionsClient, log)
		if err != nil {
			errs = append(errs, err)
		}
		if !ok {
			continue
		}

		deletedCount++
		if deletedCount == count {
			break
		}
	}

	return multierr.Combine(errs...)
}

func (r *EphemeralRunnerSetReconciler) deleteEphemeralRunnerWithActionsClient(ctx context.Context, ephemeralRunner *v1alpha1.EphemeralRunner, actionsClient actions.ActionsService, log logr.Logger) (bool, error) {
	if err := actionsClient.RemoveRunner(ctx, int64(ephemeralRunner.Status.RunnerId)); err != nil {
		actionsError := &actions.ActionsError{}
		if errors.As(err, &actionsError) &&
			actionsError.StatusCode == http.StatusBadRequest &&
			strings.Contains(actionsError.ExceptionName, "JobStillRunningException") {
			// Runner is still running a job, proceed with the next one
			return false, nil
		}

		return false, err
	}

	log.Info("Deleting ephemeral runner after removing from the service", "name", ephemeralRunner.Name, "runnerId", ephemeralRunner.Status.RunnerId)
	if err := r.Delete(ctx, ephemeralRunner); err != nil && !kerrors.IsNotFound(err) {
		return false, err
	}

	log.Info("Deleted ephemeral runner", "name", ephemeralRunner.Name, "runnerId", ephemeralRunner.Status.RunnerId)
	return true, nil
}

func (r *EphemeralRunnerSetReconciler) actionsClientFor(ctx context.Context, rs *v1alpha1.EphemeralRunnerSet) (actions.ActionsService, error) {
	secret := new(corev1.Secret)
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.EphemeralRunnerSpec.GitHubConfigSecret}, secret); err != nil {
		return nil, fmt.Errorf("failed to get secret: %w", err)
	}

	return r.ActionsClient.GetClientFromSecret(ctx, rs.Spec.EphemeralRunnerSpec.GitHubConfigUrl, rs.Namespace, secret.Data)
}

// SetupWithManager sets up the controller with the Manager.
func (r *EphemeralRunnerSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index EphemeralRunner owned by EphemeralRunnerSet so we can perform faster look ups.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.EphemeralRunner{}, ephemeralRunnerSetReconcilerOwnerKey, func(rawObj client.Object) []string {
		groupVersion := v1alpha1.GroupVersion.String()

		// grab the job object, extract the owner...
		ephemeralRunner := rawObj.(*v1alpha1.EphemeralRunner)
		owner := metav1.GetControllerOf(ephemeralRunner)
		if owner == nil {
			return nil
		}

		// ...make sure it is owned by this controller
		if owner.APIVersion != groupVersion || owner.Kind != "EphemeralRunnerSet" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.EphemeralRunnerSet{}).
		Owns(&v1alpha1.EphemeralRunner{}).
		WithEventFilter(predicate.ResourceVersionChangedPredicate{}).
		Complete(r)
}

type ephemeralRunnerStepper struct {
	items []*v1alpha1.EphemeralRunner
	index int
}

func newEphemeralRunnerStepper(pending, running []*v1alpha1.EphemeralRunner) *ephemeralRunnerStepper {
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].GetCreationTimestamp().Time.Before(pending[j].GetCreationTimestamp().Time)
	})
	sort.Slice(running, func(i, j int) bool {
		return running[i].GetCreationTimestamp().Time.Before(running[j].GetCreationTimestamp().Time)
	})

	return &ephemeralRunnerStepper{
		items: append(pending, running...),
		index: -1,
	}
}

func (s *ephemeralRunnerStepper) next() bool {
	if s.index+1 < len(s.items) {
		s.index++
		return true
	}
	return false
}

func (s *ephemeralRunnerStepper) object() *v1alpha1.EphemeralRunner {
	if s.index >= 0 && s.index < len(s.items) {
		return s.items[s.index]
	}
	return nil
}

func (s *ephemeralRunnerStepper) len() int {
	return len(s.items)
}

func categorizeEphemeralRunners(ephemeralRunnerList *v1alpha1.EphemeralRunnerList) (pendingEphemeralRunners, runningEphemeralRunners, finishedEphemeralRunners, failedEphemeralRunners, deletingEphemeralRunners []*v1alpha1.EphemeralRunner) {
	for i := range ephemeralRunnerList.Items {
		r := &ephemeralRunnerList.Items[i]
		if !r.ObjectMeta.DeletionTimestamp.IsZero() {
			deletingEphemeralRunners = append(deletingEphemeralRunners, r)
			continue
		}

		switch r.Status.Phase {
		case corev1.PodRunning:
			runningEphemeralRunners = append(runningEphemeralRunners, r)
		case corev1.PodSucceeded:
			finishedEphemeralRunners = append(finishedEphemeralRunners, r)
		case corev1.PodFailed:
			failedEphemeralRunners = append(failedEphemeralRunners, r)
		default:
			// Pending or no phase should be considered as pending.
			//
			// If field is not set, that means that the EphemeralRunner
			// did not yet have chance to update the Status.Phase field.
			pendingEphemeralRunners = append(pendingEphemeralRunners, r)
		}
	}
	return
}