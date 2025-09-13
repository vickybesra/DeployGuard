# DeployGuard

## 🎯 Project Goal
A Kubernetes Operator in Go that automatically monitors application rollouts in real-time using Prometheus metrics, and takes action (pause or rollback) when SLO violations are detected—implementing an automated safety net for production deployments.

## 🔧 Core Features

| Feature | Description |
|---------|-------------|
| **SLO-Aware Monitoring** | Watches rolling deployments and queries Prometheus for P99 latency and error rate |
| **Automatic Rollout Control** | Pauses or rolls back deployments when thresholds are exceeded |
| **Configurable Violation Strategy** | Choose between `Pause` (halt rollout) or `Rollback` (revert to previous version) |
| **Grace Period** | Configurable warmup time before metrics evaluation begins |
| **Sustained Violation Detection** | Requires consecutive failures before taking action (prevents flapping) |
| **Alert Integration** | Kubernetes Events and Slack webhook notifications |
| **Rollback Loop Prevention** | Tracks deployment generations to avoid re-monitoring its own rollbacks |

---

## 🏗️ Technical Architecture

### 1. Custom Resource Definition (CRD): `RolloutGuard`

The `RolloutGuard` CRD defines the monitoring policy for safe deployments.

- **Group**: `guard.example.com`
- **Version**: `v1`
- **Kind**: `RolloutGuard`

#### Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `targetRef` | `string` | ✅ | Name of the Deployment to monitor (same namespace) |
| `metrics.latencyQuery` | `string` | ✅ | PromQL query for latency (returns seconds, converted to ms) |
| `metrics.errorRateQuery` | `string` | ✅ | PromQL query for error rate |
| `thresholds.maxLatency` | `int` | ✅ | Maximum allowed latency in milliseconds |
| `thresholds.maxErrorRate` | `string` | ✅ | Maximum error rate (e.g., `"1%"` or `"0.01"`) |
| `thresholds.gracePeriodSeconds` | `int32` | ❌ | Warmup time before checking metrics (default: `60`) |
| `thresholds.evaluationWindowSeconds` | `int32` | ❌ | Observation window for violations (default: `30`) |
| `thresholds.consecutiveFailures` | `int32` | ❌ | Required consecutive failures before action (default: `3`) |
| `thresholds.monitoringWindowSeconds` | `int32` | ❌ | Total monitoring duration after rollout detected (default: `300`) |
| `violationStrategy` | `string` | ❌ | Action on violation: `Pause` (default) or `Rollback` |
| `alertConfig.slackWebhook` | `string` | ❌ | Slack webhook URL for notifications |

#### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `phase` | `string` | Current state: `Idle`, `Initializing`, `GracePeriod`, `Monitoring`, `Paused`, `RolledBack`, `Healthy`, `Error` |
| `lastCheckTime` | `Time` | Timestamp of last Prometheus query |
| `observedLatency` | `float64` | Last observed latency in milliseconds |
| `observedErrorRate` | `float64` | Last observed error rate as percentage |
| `message` | `string` | Human-readable status message |
| `rolloutStartTime` | `Time` | When current rollout monitoring began |
| `monitoringEndTime` | `Time` | When monitoring window expires |
| `consecutiveViolations` | `int32` | Current count of consecutive threshold breaches |
| `firstViolationTime` | `Time` | When first violation in current sequence occurred |
| `lastHealthyRevision` | `int64` | ReplicaSet revision to rollback to |
| `trackedGeneration` | `int64` | Deployment generation being monitored |

---

### 2. Controller Reconciliation Loop

The `RolloutGuardReconciler` implements a state machine with the following phases:

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         RECONCILIATION FLOW                              │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  ┌──────┐    New Rollout    ┌─────────────┐    Grace Period   ┌────────┐│
│  │ Idle │ ───────────────▶  │Initializing │ ───────────────▶  │ Grace  ││
│  └──────┘                   └─────────────┘                   │ Period ││
│      ▲                                                        └────┬───┘│
│      │                                                             │    │
│      │ Window Complete                              Grace Expires  │    │
│      │ (No Sustained Violations)                                   ▼    │
│  ┌───┴─────┐◀──────────────────────────────────────────────┌───────────┐│
│  │ Healthy │                                               │Monitoring ││
│  └─────────┘                                               └─────┬─────┘│
│                                                                  │      │
│                                           Consecutive Violations │      │
│                                                                  ▼      │
│                        ┌────────┐  Strategy=Pause   ┌─────────────────┐ │
│                        │ Paused │◀──────────────────│ Handle Violation│ │
│                        └────────┘                   └────────┬────────┘ │
│                                                              │          │
│                        ┌────────────┐  Strategy=Rollback     │          │
│                        │ RolledBack │◀───────────────────────┘          │
│                        └────────────┘                                   │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
```

#### Reconciliation Steps

1. **Fetch Resources**: Get `RolloutGuard` and target `Deployment`
2. **Check Paused State**: If deployment is paused, stop monitoring
3. **Check RolledBack State**: If already rolled back and generation unchanged, wait for new rollout
4. **Detect New Rollout**: Compare `deployment.Generation` with `trackedGeneration`
   - On new rollout: Capture previous healthy revision, initialize monitoring window
   - Skip if rollback annotation indicates this is our own rollback (within 60s)
5. **Apply Grace Period**: Wait for pods to warm up before checking metrics
6. **Monitor Metrics**: Query Prometheus for latency and error rate
7. **Track Violations**: Accumulate consecutive failures
8. **Take Action**: When `consecutiveViolations >= consecutiveFailures`:
   - **Pause**: Set `deployment.spec.paused = true`
   - **Rollback**: Copy pod template from previous ReplicaSet

#### Rollback Mechanism

The controller finds the previous healthy revision using:

1. **Strategy 1**: Find ReplicaSet with `replicas > 0` that has a lower revision (the old version being scaled down)
2. **Strategy 2**: Fall back to second-highest revision number if no RS has active replicas

Rollback annotations are added to the deployment:
- `deployguard.example.com/rolled-back-at`: Timestamp of rollback
- `deployguard.example.com/rolled-back-to`: Target revision
- `deployguard.example.com/rolled-back-from`: Original revision

---

### 3. Prometheus Integration

| Aspect | Implementation |
|--------|----------------|
| **Client** | `prometheus/client_golang/api` v1 |
| **Query Type** | Instant queries (`Query` API) |
| **Timeout** | 30 seconds |
| **Data Model** | Expects `vector` results, extracts scalar value |
| **Latency Handling** | Query returns seconds, converted to milliseconds |
| **Error Rate Handling** | Supports both percentage (`"1%"`) and ratio (`0.01`) formats |

---

### 4. Controller Setup

The controller uses predicates to minimize unnecessary reconciliations:

- **RolloutGuard changes**: Only on generation changes
- **Deployment changes**: Only on spec changes (generation bump), not status updates
- **Concurrency**: Single worker (`MaxConcurrentReconciles: 1`) to prevent race conditions

---

## 📋 Example RolloutGuard

```yaml
apiVersion: guard.example.com/v1
kind: RolloutGuard
metadata:
  name: my-app-guard
  namespace: default
spec:
  targetRef: my-deployment
  metrics:
    latencyQuery: |
      histogram_quantile(0.99, 
        sum(rate(http_request_duration_seconds_bucket{app="my-app"}[5m])) by (le)
      )
    errorRateQuery: |
      sum(rate(http_requests_total{app="my-app", status=~"5.."}[5m])) /
      sum(rate(http_requests_total{app="my-app"}[5m])) * 100
  thresholds:
    maxLatency: 500           # 500ms
    maxErrorRate: "1%"        # 1% error rate
    gracePeriodSeconds: 60    # Wait 60s for pods to warm up
    consecutiveFailures: 3    # 3 failures before action
    monitoringWindowSeconds: 300  # Monitor for 5 minutes
  violationStrategy: Rollback  # or "Pause"
  alertConfig:
    slackWebhook: https://hooks.slack.com/services/xxx/yyy/zzz
```

---

## 🔒 RBAC Permissions

The controller requires the following permissions:

| Resource | Verbs |
|----------|-------|
| `rolloutguards` | get, list, watch, create, update, patch, delete |
| `rolloutguards/status` | get, update, patch |
| `rolloutguards/finalizers` | update |
| `deployments` | get, list, watch, update, patch |
| `replicasets` | get, list, watch |
| `events` | create, patch |
