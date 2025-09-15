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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RolloutGuardSpec defines the desired state of RolloutGuard
type RolloutGuardSpec struct {
	// TargetRef references the Deployment to monitor
	// +kubebuilder:validation:Required
	TargetRef string `json:"targetRef"`

	// Metrics defines the Prometheus queries to monitor
	// +kubebuilder:validation:Required
	Metrics Metrics `json:"metrics"`

	// Thresholds defines the limits for the metrics
	// +kubebuilder:validation:Required
	Thresholds Thresholds `json:"thresholds"`

	// AlertConfig defines notification settings
	// +optional
	AlertConfig AlertConfig `json:"alertConfig,omitempty"`

	// ViolationStrategy defines what action to take when SLO violations are detected
	// Valid values: "Pause" (default), "Rollback"
	// - Pause: Stops the rollout but keeps current version
	// - Rollback: Reverts to the previous successful version
	// +optional
	// +kubebuilder:default="Pause"
	// +kubebuilder:validation:Enum=Pause;Rollback
	ViolationStrategy string `json:"violationStrategy,omitempty"`
}

// Metrics defines the PromQL queries
type Metrics struct {
	// LatencyQuery is the PromQL query for latency
	// +kubebuilder:validation:Required
	LatencyQuery string `json:"latencyQuery"`

	// ErrorRateQuery is the PromQL query for error rate
	// +kubebuilder:validation:Required
	ErrorRateQuery string `json:"errorRateQuery"`
}

// Thresholds defines the failure criteria
type Thresholds struct {
	// MaxLatency is the maximum allowed latency in milliseconds
	// +kubebuilder:validation:Minimum=0
	MaxLatency int `json:"maxLatency"`

	// MaxErrorRate is the maximum allowed error rate percentage (e.g., "1%")
	// +kubebuilder:validation:Pattern=`^\d+(?:\.\d+)?%$`
	MaxErrorRate string `json:"maxErrorRate"`

	// GracePeriodSeconds is the time to wait after rollout starts before checking metrics
	// This allows pods to warm up and start receiving traffic
	// +optional
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	GracePeriodSeconds *int32 `json:"gracePeriodSeconds,omitempty"`

	// EvaluationWindowSeconds is how long to observe violations before taking action
	// Metrics must exceed thresholds for this duration to trigger pause
	// +optional
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=10
	EvaluationWindowSeconds *int32 `json:"evaluationWindowSeconds,omitempty"`

	// ConsecutiveFailures is the number of consecutive checks that must fail
	// before pausing the deployment
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	ConsecutiveFailures *int32 `json:"consecutiveFailures,omitempty"`

	// MonitoringWindowSeconds is how long to continue monitoring after a rollout is detected
	// Even if the rollout completes, monitoring continues until this window expires
	// This catches issues in newly deployed versions
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=60
	MonitoringWindowSeconds *int32 `json:"monitoringWindowSeconds,omitempty"`
}

// AlertConfig defines where to send alerts
type AlertConfig struct {
	// SlackWebhook is the URL for Slack notifications
	// +optional
	SlackWebhook string `json:"slackWebhook,omitempty"`
}

// RolloutGuardStatus defines the observed state of RolloutGuard.
type RolloutGuardStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// Phase represents the current state of the RolloutGuard
	// Possible values: Monitoring, Paused, Healthy, Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastCheckTime is the timestamp of the last Prometheus query
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`

	// ObservedLatency is the last observed latency value in milliseconds
	// +optional
	// +kubebuilder:validation:Type=number
	ObservedLatency *float64 `json:"observedLatency,omitempty"`

	// ObservedErrorRate is the last observed error rate as a percentage
	// +optional
	// +kubebuilder:validation:Type=number
	ObservedErrorRate *float64 `json:"observedErrorRate,omitempty"`

	// Message provides additional context about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// RolloutStartTime is when the current rollout was detected
	// +optional
	RolloutStartTime *metav1.Time `json:"rolloutStartTime,omitempty"`

	// MonitoringEndTime is when monitoring should stop (RolloutStartTime + MonitoringWindow)
	// Monitoring continues even after rollout completes until this time
	// +optional
	MonitoringEndTime *metav1.Time `json:"monitoringEndTime,omitempty"`

	// FirstViolationTime is when the first violation was detected
	// +optional
	FirstViolationTime *metav1.Time `json:"firstViolationTime,omitempty"`

	// ConsecutiveViolations tracks how many checks in a row have failed
	// +optional
	ConsecutiveViolations int32 `json:"consecutiveViolations,omitempty"`

	// LastHealthyRevision is the deployment revision number of the last known healthy state
	// Used for rollback when ViolationStrategy is "Rollback"
	// +optional
	LastHealthyRevision *int64 `json:"lastHealthyRevision,omitempty"`

	// TrackedGeneration is the deployment generation we're currently monitoring
	// Used to detect when a new rollout starts
	// +optional
	TrackedGeneration int64 `json:"trackedGeneration,omitempty"`

	// conditions represent the current state of the RolloutGuard resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RolloutGuard is the Schema for the rolloutguards API
type RolloutGuard struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RolloutGuard
	// +required
	Spec RolloutGuardSpec `json:"spec"`

	// status defines the observed state of RolloutGuard
	// +optional
	Status RolloutGuardStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RolloutGuardList contains a list of RolloutGuard
type RolloutGuardList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RolloutGuard `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RolloutGuard{}, &RolloutGuardList{})
}
