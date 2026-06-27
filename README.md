# Function Mesh

Kubernetes operator for deploying MetaCall functions as a service mesh.

## Quick Start

```bash
make cluster-up          # create kind cluster + registry + Cilium
make deploy              # build images, push, install Helm chart
make port-forward-api    # expose API at localhost:9000
```

## Cluster

| Command | Description |
|---|---|
| `make cluster-up` | Create kind cluster, local registry, install Cilium |
| `make cluster-start` | Restart stopped cluster containers |
| `make cluster-stop` | Stop cluster containers (preserves state) |
| `make cluster-down` | Delete the kind cluster entirely |
| `make cluster-status` | Show nodes, pods, cluster info |

## Build & Deploy

| Command | Description |
|---|---|
| `make build` | Go build all binaries |
| `make docker-build` | Build all Docker images |
| `make docker-push` | Push all images to local registry |
| `make deploy` | Build + push + Helm install + wait for rollout |
| `make helm-install` | Install/upgrade Helm chart only |
| `make clean` | Uninstall Helm release + delete CRDs/RBAC |

## Working with Functions

```bash
# Port-forward the API first (keep running in a separate terminal)
make port-forward-api

# Deploy a function from a repo
metacall-deploy --dev --addrepo https://github.com/metacall/examples

# List all functions
kubectl get functions -A

# Describe a function
kubectl describe function <name> -n metacall-functions

# Get all resources in the mesh
kubectl get all -n metacall-system
kubectl get all -n metacall-functions

# Watch function status
kubectl get functions -n metacall-functions -w

# Call a function via router (needs port-forward-router in another terminal)
make port-forward-router
curl http://localhost:9090/v1/call/<deploy-suffix>/<function-name>
```

## Port Forwarding

| Command | Description |
|---|---|
| `make port-forward-api` | API → `localhost:9000` |
| `make port-forward-router` | Router → `localhost:9090` |

## Observability & Performance

This project uses **Cilium** as the CNI plugin with `kubeProxyReplacement=true`. This allows cross-pod function calls via `rpc_loader` to be routed directly at the **eBPF kernel level**, bypassing standard `iptables` overhead for maximum performance.

You can observe the mesh network flows using Hubble:

| Command | Description |
|---|---|
| `make hubble-ui` | Port-forward Hubble UI to `localhost:12000` |
| `make hubble-observe` | Run `hubble observe` for the functions namespace |

*Note: For maximum performance, L7 (HTTP-level) visibility is disabled by default to avoid Envoy proxy overhead. You can enable it by setting `cilium.hubble.l7Visibility: true` in `values.yaml`.*

## Tests

| Command | Description |
|---|---|
| `make test` | Run all tests |
| `make test-controller` | Controller tests only |
| `make test-router` | Router tests only |
| `make test-api` | API tests only |
