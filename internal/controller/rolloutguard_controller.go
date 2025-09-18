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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	guardv1 "github.com/milinddethe15/deployguard/api/v1"
	"github.com/milinddethe15/deployguard/internal/prometheus"
)

// Phase constants for RolloutGuard status
const (
	PhaseRolledBack = "RolledBack"
	PhaseMonitoring = "Monitoring"
	PhasePause      = "Pause"
)

// RolloutGuardReconciler reconciles a RolloutGuard object
type RolloutGuardReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Cached Prometheus client
	promClient   *prometheus.Client
	promClientMu sync.RWMutex
	promURL      string
}

// updateStatus updates the RolloutGuard status with retry on conflict
func (r *RolloutGuardReconciler) updateStatus(ctx context.Context, guard *guardv1.RolloutGuard) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get the latest version
		var latest guardv1.RolloutGuard
		if err := r.Get(ctx, types.NamespacedName{Name: guard.Name, Namespace: guard.Namespace}, &latest); err != nil {
			return err
		}
		// Copy the status from our local copy to the latest
		latest.Status = guard.Status
		return r.Status().Update(ctx, &latest)
	})
}

// +kubebuilder:rbac:groups=guard.example.com,resources=rolloutguards,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=guard.example.com,resources=rolloutguards/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=guard.example.com,resources=rolloutguards/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is the main controller loop following best practices for smart interval selection
func (r *RolloutGuardReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch RolloutGuard
	var rolloutGuard guardv1.RolloutGuard
	if err := r.Get(ctx, req.NamespacedName, &rolloutGuard); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Fetch target Deployment
	var deployment appsv1.Deployment
	targetName := rolloutGuard.Spec.TargetRef
	if err := r.Get(ctx, client.ObjectKey{Name: targetName, Namespace: req.Namespace}, &deployment); err != nil {
		logger.Error(err, "Unable to fetch target deployment", "targetRef", targetName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If Deployment is already paused, stop monitoring
	if deployment.Spec.Paused {
		rolloutGuard.Status.Phase = "Paused"
		rolloutGuard.Status.Message = "Deployment is paused"
		if err := r.updateStatus(ctx, &rolloutGuard); err != nil {
			logger.Error(err, "Failed to update status for paused deployment")
		}
		return ctrl.Result{}, nil
	}

	// If we already rolled back, don't monitor anymore until a NEW rollout happens
	// Check both the phase AND the rollback annotation to be safe
	if rolloutGuard.Status.Phase == PhaseRolledBack {
		// Only exit if the deployment hasn't changed since our rollback
		if rolloutGuard.Status.TrackedGeneration == deployment.Generation {
			logger.V(1).Info("Already rolled back, waiting for new deployment")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// Deployment has changed since rollback - clear the RolledBack state
		logger.Info("New deployment detected after rollback, resuming monitoring")
	}

	// Fixed requeue interval
	const requeueAfter = 30 * time.Second

	// 4. If deployment is not in rollout and we're not in cooldown period
	rollingOut := r.isRollingOut(&deployment)
	inCooldown := r.inCooldownPeriod(&rolloutGuard)

	logger.V(1).Info("Checking deployment state",
		"isRollingOut", rollingOut,
		"inCooldownPeriod", inCooldown,
		"requeueAfter", requeueAfter.String())

	if !rollingOut && !inCooldown {
		// Check if we just completed a monitoring window (had RolloutStartTime but no longer in cooldown)
		justCompletedMonitoring := rolloutGuard.Status.RolloutStartTime != nil

		if justCompletedMonitoring {
			// Monitoring window completed successfully (no sustained violations)
			logger.Info("Monitoring window completed successfully", "deployment", deployment.Name)
			rolloutGuard.Status.RolloutStartTime = nil
			rolloutGuard.Status.MonitoringEndTime = nil
			rolloutGuard.Status.ConsecutiveViolations = 0
			rolloutGuard.Status.FirstViolationTime = nil
			rolloutGuard.Status.Phase = "Healthy"
			rolloutGuard.Status.Message = "✅ Rollout completed successfully. Monitoring window passed with no sustained violations."
		} else {
			// Not in any rollout, just idle
			rolloutGuard.Status.Phase = "Idle"
			rolloutGuard.Status.Message = "No active rollout, watching for changes"
		}
		if err := r.updateStatus(ctx, &rolloutGuard); err != nil {
			logger.Error(err, "Failed to update status")
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// Detect if this is a NEW rollout by checking deployment generation
	isNewRollout := rollingOut && rolloutGuard.Status.TrackedGeneration != deployment.Generation

	// Check if this "new rollout" is actually our own rollback
	// by looking at the rollback annotation timestamp
	if isNewRollout {
		if rollbackTime, ok := deployment.Annotations["deployguard.example.com/rolled-back-at"]; ok {
			if t, err := time.Parse(time.RFC3339, rollbackTime); err == nil {
				// If rollback was performed within the last 60 seconds, this is our rollback
				if time.Since(t) < 60*time.Second {
					logger.Info("Skipping monitoring for our own rollback",
						"rollbackTime", rollbackTime,
						"generation", deployment.Generation)
					// Update TrackedGeneration to this rollback's generation
					rolloutGuard.Status.TrackedGeneration = deployment.Generation
					rolloutGuard.Status.Phase = PhaseRolledBack
					rolloutGuard.Status.Message = "Deployment was rolled back due to SLO violations"
					if err := r.updateStatus(ctx, &rolloutGuard); err != nil {
						logger.Error(err, "Failed to update status after rollback skip")
					}
					return ctrl.Result{RequeueAfter: requeueAfter}, nil
				}
			}
		}
	}

	// Initialize or reset monitoring window for new rollout
	if isNewRollout {
		now := metav1.Now()
		monitoringWindow := int32(15) // default 5 minutes
		if rolloutGuard.Spec.Thresholds.MonitoringWindowSeconds != nil {
			monitoringWindow = *rolloutGuard.Spec.Thresholds.MonitoringWindowSeconds
		}
		endTime := metav1.NewTime(now.Add(time.Duration(monitoringWindow) * time.Second))

		// CRITICAL: Capture the PREVIOUS healthy revision BEFORE resetting state
		// This is the revision we'll rollback to if the new deployment has issues
		previousRevision := r.findPreviousHealthyRevision(ctx, &deployment)
		if previousRevision > 0 {
			rolloutGuard.Status.LastHealthyRevision = &previousRevision
			logger.Info("Captured previous healthy revision for potential rollback",
				"previousRevision", previousRevision)
		}

		// Reset all monitoring state for the new rollout
		rolloutGuard.Status.RolloutStartTime = &now
		rolloutGuard.Status.MonitoringEndTime = &endTime
		rolloutGuard.Status.ConsecutiveViolations = 0
		rolloutGuard.Status.FirstViolationTime = nil
		rolloutGuard.Status.TrackedGeneration = deployment.Generation
		rolloutGuard.Status.Phase = "Initializing"
		rolloutGuard.Status.Message = "New rollout detected, starting monitoring"

		logger.Info("New rollout detected, initializing monitoring",
			"deployment", deployment.Name,
			"generation", deployment.Generation,
			"windowSeconds", monitoringWindow,
			"monitoringUntil", endTime.Format(time.RFC3339))

		if err := r.updateStatus(ctx, &rolloutGuard); err != nil {
			logger.Error(err, "Failed to update status for rollout start")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		// Return immediately to ensure we start fresh with the new state
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Apply grace period - don't check metrics too early
	// But skip grace period check if we've already started evaluating (to avoid regressing)
	alreadyEvaluating := rolloutGuard.Status.Phase == "Evaluating" || rolloutGuard.Status.ConsecutiveViolations > 0
	if !alreadyEvaluating && rolloutGuard.Status.RolloutStartTime != nil {
		gracePeriod := int32(60) // default 60 seconds
		if rolloutGuard.Spec.Thresholds.GracePeriodSeconds != nil {
			gracePeriod = *rolloutGuard.Spec.Thresholds.GracePeriodSeconds
		}

		elapsedSinceRollout := time.Since(rolloutGuard.Status.RolloutStartTime.Time)
		if elapsedSinceRollout < time.Duration(gracePeriod)*time.Second {
			remainingGrace := time.Duration(gracePeriod)*time.Second - elapsedSinceRollout

			logger.Info("Within grace period, waiting for pods to warm up",
				"elapsed", elapsedSinceRollout.Seconds(),
				"gracePeriod", gracePeriod,
				"remaining", remainingGrace.Seconds())

			// Only update status if phase changed to avoid triggering more reconciles
			if rolloutGuard.Status.Phase != "GracePeriod" {
				rolloutGuard.Status.Phase = "GracePeriod"
				rolloutGuard.Status.Message = fmt.Sprintf("Waiting %.0fs for pods to warm up",
					float64(gracePeriod))
				if err := r.updateStatus(ctx, &rolloutGuard); err != nil {
					logger.Error(err, "Failed to update grace period status")
				}
			}
			return ctrl.Result{RequeueAfter: remainingGrace}, nil
		}
	}

	// 5. Perform monitoring
	needsAction, err := r.monitorDeployment(ctx, &deployment, &rolloutGuard)
	if err != nil {
		logger.Error(err, "Failed to monitor deployment")
		rolloutGuard.Status.Phase = "Error"
		rolloutGuard.Status.Message = fmt.Sprintf("Monitoring error: %v", err)
		if updateErr := r.updateStatus(ctx, &rolloutGuard); updateErr != nil {
			logger.Error(updateErr, "Failed to update status after error")
		}
		// Return requeue with longer delay for transient errors
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// 6. Take action if needed
	if needsAction {
		newGeneration, err := r.handleViolation(ctx, &rolloutGuard, &deployment)
		if err != nil {
			logger.Error(err, "Failed to handle violation")
			rolloutGuard.Status.Phase = "Error"
			rolloutGuard.Status.Message = fmt.Sprintf("Failed to handle violation: %v", err)
		} else if newGeneration > 0 {
			// Rollback was successful - track the new generation so we don't re-monitor
			// our own rollback as a "new rollout"
			rolloutGuard.Status.TrackedGeneration = newGeneration
			logger.Info("Tracking rollback generation to prevent re-monitoring",
				"newGeneration", newGeneration)
		}
		r.sendAlert(ctx, &rolloutGuard)

		if err := r.updateStatus(ctx, &rolloutGuard); err != nil {
			logger.Error(err, "Failed to update status after violation handling")
		}
		// After action, check back in 1 minute
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Check if monitoring window has expired (safety check - normally handled above)
	if rolloutGuard.Status.MonitoringEndTime != nil &&
		time.Now().After(rolloutGuard.Status.MonitoringEndTime.Time) {
		logger.Info("Monitoring window expired during cooldown", "deployment", deployment.Name)
		rolloutGuard.Status.RolloutStartTime = nil
		rolloutGuard.Status.MonitoringEndTime = nil
		rolloutGuard.Status.ConsecutiveViolations = 0
		rolloutGuard.Status.FirstViolationTime = nil
		rolloutGuard.Status.Phase = "Healthy"
		rolloutGuard.Status.Message = "✅ Rollout completed successfully. Monitoring window passed with no sustained violations."
	}

	// Update status
	if err := r.updateStatus(ctx, &rolloutGuard); err != nil {
		logger.Error(err, "Failed to update status")
	}

	// 7. Calculate requeue interval
	// Use faster requeue when accumulating violations to catch threshold before window expires
	requeue := requeueAfter
	if rolloutGuard.Status.ConsecutiveViolations > 0 {
		requeue = 10 * time.Second // Faster checks during violation accumulation
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// isRollingOut checks if the deployment is currently rolling out
func (r *RolloutGuardReconciler) isRollingOut(deployment *appsv1.Deployment) bool {
	return deployment.Status.ObservedGeneration != deployment.Generation ||
		deployment.Status.UpdatedReplicas != deployment.Status.Replicas
}

// inCooldownPeriod checks if we're still in the monitoring window after a rollout
func (r *RolloutGuardReconciler) inCooldownPeriod(guard *guardv1.RolloutGuard) bool {
	if guard.Status.MonitoringEndTime == nil {
		return false
	}
	return time.Now().Before(guard.Status.MonitoringEndTime.Time)
}

// findPreviousHealthyRevision finds the revision that was RUNNING before the current rollout
// This looks for the ReplicaSet that currently has replicas (the one being scaled down)
// OR falls back to the second-highest revision if no RS has replicas yet
func (r *RolloutGuardReconciler) findPreviousHealthyRevision(ctx context.Context, deployment *appsv1.Deployment) int64 {
	logger := log.FromContext(ctx)

	var replicaSets appsv1.ReplicaSetList
	if err := r.List(ctx, &replicaSets,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels(deployment.Spec.Selector.MatchLabels),
	); err != nil {
		logger.Error(err, "Failed to list ReplicaSets for previous revision lookup")
		return 0
	}

	// Get the current deployment revision (the new rollout we're about to monitor)
	currentRevStr, hasCurrentRev := deployment.Annotations["deployment.kubernetes.io/revision"]
	var currentRev int64
	if hasCurrentRev {
		_, _ = fmt.Sscanf(currentRevStr, "%d", &currentRev)
	}

	// Strategy 1: Find the RS that currently has replicas AND is NOT the newest one
	// During a rollout, the OLD RS will still have some replicas
	var runningRS *appsv1.ReplicaSet
	var runningRev int64

	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		revisionStr, ok := rs.Annotations["deployment.kubernetes.io/revision"]
		if !ok {
			continue
		}
		var rev int64
		if _, err := fmt.Sscanf(revisionStr, "%d", &rev); err != nil {
			continue
		}

		// Look for RS with replicas that is NOT the newest revision
		// The newest RS (being rolled out to) might also have replicas, so we want the OLDER one
		if rs.Status.Replicas > 0 {
			if runningRS == nil || rev < runningRev {
				// We want the OLDER running RS (lower revision), as that's the "previous" one
				runningRS = rs
				runningRev = rev
			}
		}
	}

	if runningRS != nil && runningRev != currentRev {
		logger.V(1).Info("Found running RS as previous revision",
			"currentRevision", currentRev,
			"previousRevision", runningRev,
			"replicaSet", runningRS.Name)
		return runningRev
	}

	// Strategy 2: If all RS have 0 replicas (rollout completed quickly),
	// find the second-highest revision
	var highestRev, secondHighestRev int64 = 0, 0

	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		revisionStr, ok := rs.Annotations["deployment.kubernetes.io/revision"]
		if !ok {
			continue
		}
		var rev int64
		if _, err := fmt.Sscanf(revisionStr, "%d", &rev); err != nil {
			continue
		}

		if rev > highestRev {
			secondHighestRev = highestRev
			highestRev = rev
		} else if rev > secondHighestRev {
			secondHighestRev = rev
		}
	}

	logger.V(1).Info("Found previous revision via second-highest",
		"highestRevision", highestRev,
		"previousRevision", secondHighestRev)

	return secondHighestRev
}

// monitorDeployment checks metrics and returns true if action is needed
func (r *RolloutGuardReconciler) monitorDeployment(ctx context.Context, _ *appsv1.Deployment, guard *guardv1.RolloutGuard) (bool, error) {
	logger := log.FromContext(ctx)

	// Get cached Prometheus client
	promClient, err := r.getPromClient()
	if err != nil {
		return false, fmt.Errorf("failed to create prometheus client: %w", err)
	}

	// Update last check time
	now := metav1.Now()
	guard.Status.LastCheckTime = &now
	guard.Status.Phase = PhaseMonitoring

	// Check latency
	latency, err := promClient.QueryScalar(ctx, guard.Spec.Metrics.LatencyQuery)
	if err != nil {
		return false, fmt.Errorf("latency query failed: %w", err)
	}

	// Check error rate
	errorRate, err := promClient.QueryScalar(ctx, guard.Spec.Metrics.ErrorRateQuery)
	if err != nil {
		return false, fmt.Errorf("error rate query failed: %w", err)
	}

	// Convert latency to milliseconds and store in status
	latencyMs := latency * 1000
	guard.Status.ObservedLatency = &latencyMs

	// Normalize error rate to percentage
	errorRatePercent := errorRate
	if errorRate <= 1 {
		errorRatePercent = errorRate * 100
	}
	guard.Status.ObservedErrorRate = &errorRatePercent

	// Parse thresholds
	maxErrorRate, err := parseErrorRate(guard.Spec.Thresholds.MaxErrorRate)
	if err != nil {
		return false, fmt.Errorf("invalid error rate threshold: %w", err)
	}

	// Check for violations
	latencyViolation := latencyMs > float64(guard.Spec.Thresholds.MaxLatency)
	errorRateViolation := errorRatePercent > maxErrorRate
	hasViolation := latencyViolation || errorRateViolation

	logger.Info("Metrics check",
		"latency_ms", latencyMs,
		"max_latency_ms", guard.Spec.Thresholds.MaxLatency,
		"error_rate", errorRatePercent,
		"max_error_rate", maxErrorRate,
		"violation", hasViolation)

	if hasViolation {
		return r.trackViolation(ctx, guard)
	}

	// Metrics are healthy - reset violation tracking
	if guard.Status.ConsecutiveViolations > 0 {
		logger.Info("Metrics recovered, resetting violation counters")
	}
	guard.Status.ConsecutiveViolations = 0
	guard.Status.FirstViolationTime = nil

	// During monitoring window, use PhaseMonitoring to indicate we're still watching
	// Only use "Healthy" after monitoring window completes (handled in main Reconcile)
	guard.Status.Phase = PhaseMonitoring
	guard.Status.Message = fmt.Sprintf("Metrics OK (Latency: %.2fms, ErrorRate: %.2f%%). Monitoring until window ends.",
		latencyMs, errorRatePercent)

	// NOTE: We do NOT update LastHealthyRevision here - it is captured at rollout START
	// to remember the previous working version. If this new rollout is healthy, the
	// monitoring window will complete and the next rollout will capture the new revision.

	return false, nil
}

// trackViolation tracks consecutive violations and returns true if action should be taken
func (r *RolloutGuardReconciler) trackViolation(ctx context.Context, guard *guardv1.RolloutGuard) (bool, error) {
	logger := log.FromContext(ctx)

	// Track first violation time
	if guard.Status.FirstViolationTime == nil {
		now := metav1.Now()
		guard.Status.FirstViolationTime = &now
	}
	guard.Status.ConsecutiveViolations++

	// Get threshold with default
	requiredFailures := int32(3)
	if guard.Spec.Thresholds.ConsecutiveFailures != nil {
		requiredFailures = *guard.Spec.Thresholds.ConsecutiveFailures
	}

	violationDuration := time.Since(guard.Status.FirstViolationTime.Time)

	// Check if we've reached the threshold for action (consecutive failures only)
	if guard.Status.ConsecutiveViolations >= requiredFailures {
		logger.Info("Sustained violation detected, taking action",
			"consecutiveFailures", guard.Status.ConsecutiveViolations,
			"duration", violationDuration.Seconds(),
			"strategy", guard.Spec.ViolationStrategy)

		strategy := guard.Spec.ViolationStrategy
		if strategy == "" {
			strategy = PhasePause
		}

		if strategy == "Rollback" {
			guard.Status.Phase = PhaseRolledBack
			guard.Status.Message = fmt.Sprintf("Deployment rolled back: sustained violation for %.0fs (%d consecutive checks)",
				violationDuration.Seconds(), guard.Status.ConsecutiveViolations)
		} else {
			guard.Status.Phase = "Paused"
			guard.Status.Message = fmt.Sprintf("Deployment paused: sustained violation for %.0fs (%d consecutive checks)",
				violationDuration.Seconds(), guard.Status.ConsecutiveViolations)
		}

		// End monitoring after action
		// NOTE: Do NOT reset TrackedGeneration here - it will be updated with the
		// new rollback generation in handleViolation to prevent re-monitoring
		guard.Status.RolloutStartTime = nil
		guard.Status.MonitoringEndTime = nil
		guard.Status.ConsecutiveViolations = 0
		guard.Status.FirstViolationTime = nil

		return true, nil
	}

	// Still accumulating violations - stay in Monitoring phase but show warning
	guard.Status.Phase = PhaseMonitoring
	guard.Status.Message = fmt.Sprintf("⚠️ Violation detected (check %d/%d). Will act after %d consecutive failures.",
		guard.Status.ConsecutiveViolations, requiredFailures, requiredFailures)

	logger.Info("Violation detected, accumulating",
		"consecutive", guard.Status.ConsecutiveViolations,
		"required", requiredFailures)

	return false, nil
}

// handleViolation takes action based on the violation strategy
// Returns the new deployment generation after action (for rollback) so we can track it
func (r *RolloutGuardReconciler) handleViolation(ctx context.Context, guard *guardv1.RolloutGuard, deployment *appsv1.Deployment) (int64, error) {
	logger := log.FromContext(ctx)

	strategy := guard.Spec.ViolationStrategy
	if strategy == "" {
		strategy = PhasePause
	}

	switch strategy {
	case "Rollback":
		logger.Info("Performing rollback due to SLO violation")
		return r.rollbackDeployment(ctx, guard, deployment)
	case PhasePause:
		logger.Info("Pausing deployment due to SLO violation")
		return 0, r.pauseDeployment(ctx, deployment)
	default:
		logger.Info("Unknown strategy, defaulting to pause", "strategy", strategy)
		return 0, r.pauseDeployment(ctx, deployment)
	}
}

// pauseDeployment sets spec.paused = true on the deployment
func (r *RolloutGuardReconciler) pauseDeployment(ctx context.Context, deployment *appsv1.Deployment) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &latest); err != nil {
			return err
		}
		if latest.Spec.Paused {
			return nil // Already paused
		}
		latest.Spec.Paused = true
		return r.Update(ctx, &latest)
	})
}

// rollbackDeployment reverts to the previous working revision
// Returns the new deployment generation after rollback so we can track it
func (r *RolloutGuardReconciler) rollbackDeployment(ctx context.Context, guard *guardv1.RolloutGuard, deployment *appsv1.Deployment) (int64, error) {
	logger := log.FromContext(ctx)

	// Find all ReplicaSets for this deployment
	var replicaSets appsv1.ReplicaSetList
	if err := r.List(ctx, &replicaSets,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels(deployment.Spec.Selector.MatchLabels),
	); err != nil {
		return 0, fmt.Errorf("failed to list replicasets: %w", err)
	}

	if len(replicaSets.Items) < 2 {
		logger.Info("No previous ReplicaSet available for rollback, pausing instead")
		return 0, r.pauseDeployment(ctx, deployment)
	}

	// Find the current and previous ReplicaSets by revision
	var currentRS, previousRS *appsv1.ReplicaSet
	var currentRev, previousRev int64 = 0, 0

	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		revisionStr, ok := rs.Annotations["deployment.kubernetes.io/revision"]
		if !ok {
			continue
		}
		var rev int64
		if _, err := fmt.Sscanf(revisionStr, "%d", &rev); err != nil {
			continue
		}

		if rev > currentRev {
			// Current becomes previous
			previousRev = currentRev
			previousRS = currentRS
			// This becomes current
			currentRev = rev
			currentRS = rs
		} else if rev > previousRev {
			previousRev = rev
			previousRS = rs
		}
	}

	// First try to use LastHealthyRevision if it exists and we have that RS
	if guard.Status.LastHealthyRevision != nil {
		targetRevision := *guard.Status.LastHealthyRevision
		for i := range replicaSets.Items {
			rs := &replicaSets.Items[i]
			if revisionStr, ok := rs.Annotations["deployment.kubernetes.io/revision"]; ok {
				var rev int64
				if _, err := fmt.Sscanf(revisionStr, "%d", &rev); err == nil && rev == targetRevision {
					logger.Info("Rolling back to last healthy revision",
						"targetRevision", targetRevision,
						"targetReplicaSet", rs.Name)
					return r.applyRollback(ctx, deployment, rs, targetRevision)
				}
			}
		}
		logger.Info("Last healthy revision ReplicaSet not found, trying previous revision",
			"lastHealthyRevision", targetRevision)
	}

	// Fall back to previous revision
	if previousRS == nil {
		logger.Info("No previous ReplicaSet found, pausing instead")
		return 0, r.pauseDeployment(ctx, deployment)
	}

	logger.Info("Rolling back to previous revision",
		"currentRevision", currentRev,
		"targetRevision", previousRev,
		"targetReplicaSet", previousRS.Name)

	return r.applyRollback(ctx, deployment, previousRS, previousRev)
}

// applyRollback applies the rollback by copying the template from target ReplicaSet
// Returns the new deployment generation after rollback so we can track it
func (r *RolloutGuardReconciler) applyRollback(ctx context.Context, deployment *appsv1.Deployment, targetRS *appsv1.ReplicaSet, targetRevision int64) (int64, error) {
	logger := log.FromContext(ctx)

	var newGeneration int64
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &latest); err != nil {
			return err
		}

		// Store the current revision before rollback
		currentRevision := latest.Annotations["deployment.kubernetes.io/revision"]

		latest.Spec.Template = targetRS.Spec.Template
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations["deployguard.example.com/rolled-back-at"] = time.Now().Format(time.RFC3339)
		latest.Annotations["deployguard.example.com/rolled-back-to"] = fmt.Sprintf("%d", targetRevision)
		latest.Annotations["deployguard.example.com/rolled-back-from"] = currentRevision

		if err := r.Update(ctx, &latest); err != nil {
			return err
		}

		// Capture the new generation after update
		newGeneration = latest.Generation
		return nil
	})

	if err != nil {
		logger.Error(err, "Failed to apply rollback", "targetRevision", targetRevision)
		return 0, err
	}

	logger.Info("Rollback applied successfully",
		"deployment", deployment.Name,
		"targetRevision", targetRevision,
		"targetReplicaSet", targetRS.Name,
		"newGeneration", newGeneration)
	return newGeneration, nil
}

// sendAlert sends notifications about the violation
func (r *RolloutGuardReconciler) sendAlert(ctx context.Context, guard *guardv1.RolloutGuard) {
	logger := log.FromContext(ctx)

	var latencyMs, errorRate float64
	if guard.Status.ObservedLatency != nil {
		latencyMs = *guard.Status.ObservedLatency
	}
	if guard.Status.ObservedErrorRate != nil {
		errorRate = *guard.Status.ObservedErrorRate
	}

	strategy := guard.Spec.ViolationStrategy
	if strategy == "" {
		strategy = PhasePause
	}

	// Send Kubernetes event
	if r.Recorder != nil {
		r.Recorder.Eventf(guard, "Warning", "SLOViolation",
			"SLO violation: Latency %.2fms, ErrorRate %.2f%%. Action: %s",
			latencyMs, errorRate, strategy)
	}

	// Send Slack alert if configured
	if webhook := guard.Spec.AlertConfig.SlackWebhook; webhook != "" {
		message := fmt.Sprintf("🚨 SLO violation detected!\nDeployment: %s\nLatency: %.2fms\nError Rate: %.2f%%\nAction: %s",
			guard.Spec.TargetRef, latencyMs, errorRate, strategy)
		if err := sendSlackNotification(ctx, webhook, message); err != nil {
			logger.Error(err, "Failed to send Slack notification")
		}
	}
}

// getPromClient returns a cached Prometheus client
func (r *RolloutGuardReconciler) getPromClient() (*prometheus.Client, error) {
	url := getPrometheusURL()

	r.promClientMu.RLock()
	if r.promClient != nil && r.promURL == url {
		cachedClient := r.promClient
		r.promClientMu.RUnlock()
		return cachedClient, nil
	}
	r.promClientMu.RUnlock()

	r.promClientMu.Lock()
	defer r.promClientMu.Unlock()

	// Double-check after acquiring write lock
	if r.promClient != nil && r.promURL == url {
		return r.promClient, nil
	}

	newClient, err := prometheus.NewClient(url)
	if err != nil {
		return nil, err
	}

	r.promClient = newClient
	r.promURL = url
	return newClient, nil
}

// Helper functions

func getPrometheusURL() string {
	if url := os.Getenv("PROMETHEUS_URL"); url != "" {
		return url
	}
	// Default to common Prometheus service name in monitoring namespace
	return "http://prometheus-server.monitoring.svc.cluster.local:80"
}

func parseErrorRate(threshold string) (float64, error) {
	clean := strings.TrimSpace(threshold)
	hasPercent := strings.HasSuffix(clean, "%")
	clean = strings.TrimSuffix(clean, "%")

	value, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid error rate threshold %q: %w", threshold, err)
	}

	// If user specified "1%" or "5%", the value is already in percentage
	// If user specified "0.01" or "0.05" (no %), assume it's a ratio and convert
	if !hasPercent && value <= 1.0 {
		return value * 100, nil
	}
	return value, nil
}

func sendSlackNotification(ctx context.Context, webhookURL, message string) error {
	payload := map[string]string{"text": message}
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *RolloutGuardReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only trigger on deployment generation changes (spec changes that indicate new rollout)
	deploymentPredicate := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldDep := e.ObjectOld.(*appsv1.Deployment)
			newDep := e.ObjectNew.(*appsv1.Deployment)
			// Only reconcile if spec changed (generation bump)
			// Status changes are handled by periodic requeue
			return oldDep.Generation != newDep.Generation
		},
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&guardv1.RolloutGuard{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.findRolloutGuardsForDeployment),
			builder.WithPredicates(deploymentPredicate),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1, // Prevent parallel reconciles for the same resource
		}).
		Named("rolloutguard").
		Complete(r)
}

// findRolloutGuardsForDeployment maps Deployment changes to RolloutGuard reconcile requests
func (r *RolloutGuardReconciler) findRolloutGuardsForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	deployment := obj.(*appsv1.Deployment)

	var rolloutGuards guardv1.RolloutGuardList
	if err := r.List(ctx, &rolloutGuards, client.InNamespace(deployment.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, guard := range rolloutGuards.Items {
		if guard.Spec.TargetRef == deployment.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      guard.Name,
					Namespace: guard.Namespace,
				},
			})
		}
	}

	return requests
}
