# Testing Guide

This guide helps you manually test the DeployGuard operator using a local Kind cluster and a test application.

## Prerequisites

- [Kind](https://kind.sigs.k8s.io/)
- [Docker](https://www.docker.com/)
- [Kubectl](https://kubernetes.io/docs/tasks/tools/)
- [Helm](https://helm.sh/)

## 1. Setup Cluster

Create a Kind cluster:
```bash
kind create cluster --name demo
```

## 2. Install Prometheus

Install Prometheus using Helm:
```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm install prometheus prometheus-community/prometheus --namespace monitoring --create-namespace
```

Wait for Prometheus to be ready:
```bash
kubectl wait --for=condition=available deployment/prometheus-server -n monitoring --timeout=120s
```

## 3. Build and Load Images

### Build Operator
```bash
make docker-build IMG=controller:latest
kind load docker-image controller:latest --name demo
```

### Build Test App
```bash
cd test/app
docker build -t testapp:latest .
kind load docker-image testapp:latest --name demo
cd ../..
```

## 4. Deploy Operator

Install CRDs and deploy the operator:
```bash
make install
make deploy IMG=controller:latest
```

Wait for the controller to be ready:
```bash
kubectl rollout status deployment/deployguard-controller-manager -n deployguard-system --timeout=60s
```

## 5. Deploy Test App and Guard

Deploy the sample application and RolloutGuard:
```bash
kubectl apply -f config/samples/sample_deployment.yaml
kubectl apply -f config/samples/guard_v1_rolloutguard.yaml
```

Verify both are running:
```bash
kubectl get deployment sample-deployment
kubectl get rolloutguard rolloutguard-sample
```

## 6. Generate Traffic

The test app exposes Prometheus metrics. You need to generate traffic to populate metrics:

```bash
# Port forward to the service
kubectl port-forward svc/sample-service 8080:80 &

# Generate continuous traffic (run in background or separate terminal)
while true; do curl -s localhost:8080 > /dev/null; sleep 0.1; done
```

## 7. Test Scenarios

### Watch Controller Logs (keep in separate terminal)
```bash
kubectl logs -n deployguard-system deploy/deployguard-controller-manager -f
```

### Scenario A: Healthy Rollout

1. Trigger a normal rollout:
   ```bash
   kubectl rollout restart deployment/sample-deployment
   ```
2. Watch the controller logs. You should see:
   - "New rollout detected, initializing monitoring"
   - "Within grace period, waiting for pods to warm up"
   - "Metrics check" with violation=false
   - Eventually "Monitoring window completed successfully"

3. Check RolloutGuard status:
   ```bash
   kubectl get rolloutguard rolloutguard-sample -o yaml
   ```

### Scenario B: Latency Violation (Rollback)

1. Trigger a rollout that simulates high latency:
   ```bash
   kubectl set env deployment/sample-deployment MODE=slow
   ```

2. Generate traffic to the slow endpoint:
   ```bash
   while true; do curl -s localhost:8080 > /dev/null; sleep 0.1; done
   ```

3. Watch the logs. You should see:
   - "Metrics check" with latency exceeding threshold (>500ms)
   - "Violation detected, accumulating" (counts up to 3)
   - "Sustained violation detected, taking action"
   - "Rolling back to last healthy revision"
   - "Rollback applied successfully"

4. Verify the rollback:
   ```bash
   # Check the deployment was rolled back (MODE should be gone or reset)
   kubectl get deployment sample-deployment -o yaml | grep -A2 "env:"
   
   # Check RolloutGuard status shows RolledBack
   kubectl get rolloutguard rolloutguard-sample -o jsonpath='{.status.phase}'
   ```

### Scenario C: Error Rate Violation (Rollback)

1. Reset the deployment to normal first:
   ```bash
   kubectl set env deployment/sample-deployment MODE=normal
   # Wait for healthy state
   sleep 60
   ```

2. Trigger a rollout that simulates errors:
   ```bash
   kubectl set env deployment/sample-deployment MODE=error
   ```

3. Generate traffic:
   ```bash
   while true; do curl -s localhost:8080 > /dev/null; sleep 0.1; done
   ```

4. Watch logs. The deployment should be rolled back due to error rate > 1%.

### Scenario D: Pause Strategy

To test the Pause strategy instead of Rollback:

1. Update the RolloutGuard to use Pause:
   ```bash
   kubectl patch rolloutguard rolloutguard-sample --type=merge -p '{"spec":{"violationStrategy":"Pause"}}'
   ```

2. Trigger a violation (slow or error mode)

3. The deployment will be paused instead of rolled back:
   ```bash
   kubectl get deployment sample-deployment -o jsonpath='{.spec.paused}'
   # Should output: true
   ```

4. Resume when ready:
   ```bash
   kubectl rollout resume deployment/sample-deployment
   ```

## 8. Check Status

View RolloutGuard status at any time:
```bash
kubectl get rolloutguard rolloutguard-sample -o yaml
```

Key status fields:
- `phase`: Current state (Idle, Monitoring, RolledBack, Paused, Healthy)
- `observedLatency`: Last observed P99 latency in milliseconds
- `observedErrorRate`: Last observed error rate as percentage
- `consecutiveViolations`: Number of consecutive threshold breaches
- `lastHealthyRevision`: ReplicaSet revision used for rollback

## Cleanup

```bash
# Delete the test resources
kubectl delete -f config/samples/

# Undeploy the operator
make undeploy

# Delete the Kind cluster
kind delete cluster --name demo
```