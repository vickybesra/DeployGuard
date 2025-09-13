# DeployGuard

> **Note**: This project is for demonstration and learning purposes only.

A Kubernetes Operator that monitors application rollouts using Prometheus metrics and automatically pauses or rolls back deployments when SLO violations are detected.

https://github.com/user-attachments/assets/6af70f2a-198b-4018-a934-8b6f2eb7706f

## Features

- **SLO-Aware Monitoring** — Queries Prometheus for P99 latency and error rate during rollouts
- **Automatic Rollback** — Reverts to the previous healthy version when thresholds are exceeded
- **Grace Period** — Waits for pods to warm up before evaluating metrics
- **Sustained Violation Detection** — Requires consecutive failures to prevent false positives

## Quick Start

```bash
# Install CRDs
make install

# Deploy controller
make deploy IMG=ghcr.io/milinddethe15/deployguard:latest

# Create a RolloutGuard
kubectl apply -f config/samples/guard_v1_rolloutguard.yaml
```

## Example

```yaml
apiVersion: guard.example.com/v1
kind: RolloutGuard
metadata:
  name: my-app-guard
spec:
  targetRef: my-deployment
  metrics:
    latencyQuery: histogram_quantile(0.99, sum(rate(http_request_duration_seconds_bucket[5m])) by (le))
    errorRateQuery: sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) * 100
  thresholds:
    maxLatency: 500        # milliseconds
    maxErrorRate: "1%"
  violationStrategy: Rollback
```

## Documentation

See [project.md](project.md) for detailed architecture and configuration options.
