# Omni Autoscaler

A Kubernetes controller that automatically scales Omni Machine Sets based on cluster workload.

## Features

- **Pending Pod Detection**: Scales up when pods can't be scheduled due to insufficient resources
- **Node Utilization Monitoring**: Scales down when nodes are underutilized
- **Min/Max Bounds**: Configurable minimum and maximum sizes per Machine Set
- **Cooldown Periods**: Prevents scaling thrashing
- **Multiple Machine Sets**: Configure different scaling parameters per Machine Set

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Kubernetes Cluster                       │
│  ┌─────────────────────┐      ┌──────────────────────────┐  │
│  │  omni-autoscaler    │      │     Pending Pods         │  │
│  │  (Deployment)       │─────▶│  (watch for unschedulable│  │
│  │                     │      │   pods & node resources) │  │
│  └──────────┬──────────┘      └──────────────────────────┘  │
│             │                                                │
└─────────────┼────────────────────────────────────────────────┘
              │ Omni API (gRPC)
              ▼
       ┌──────────────┐
       │  Omni Server │
       │  (Machine    │
       │   Sets)      │
       └──────────────┘
```

## Configuration

Create a ConfigMap with your scaling configuration:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: omni-autoscaler-config
  namespace: omni-autoscaler
data:
  config.yaml: |
    omniEndpoint: "https://omni.example.com"
    clusterName: "my-cluster"
    syncInterval: 30s
    cooldowns:
      scaleUp: 2m
      scaleDown: 10m
    machineSets:
      - name: "worker-medium"
        minSize: 2
        maxSize: 10
        scaleUpPendingPods: 1
        scaleDownUtilization: 0.3
```

## Deployment

### Prerequisites

1. An Omni service account with permissions to manage Machine Sets
2. The service account key stored as a Kubernetes Secret:

```bash
kubectl create secret generic omni-credentials \
  --namespace omni-autoscaler \
  --from-literal=serviceAccountKey='your-service-account-key'
```

### Using Kustomize

```bash
# Deploy base configuration
kubectl apply -k deploy/base

# Or use a cluster-specific overlay
kubectl apply -k deploy/overlays/hyperion
```

### Using Flux

Reference from your Flux configuration:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: omni-autoscaler
  namespace: flux-system
spec:
  sourceRef:
    kind: GitRepository
    name: omni-autoscaler
  path: ./deploy/overlays/hyperion
  prune: true
```

## Development

### Local Development

```bash
# Install dependencies
go mod download

# Run tests
go test -v ./...

# Build locally
make build

# Run locally (requires KUBECONFIG)
export OMNI_SERVICE_ACCOUNT_KEY="your-key"
./bin/omni-autoscaler -config config.yaml -log-level debug
```

### Building Docker Image

```bash
make docker-build
make docker-push
```

## License

MIT
