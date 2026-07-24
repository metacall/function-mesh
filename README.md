# Function Mesh

Kubernetes operator for deploying MetaCall functions as a service mesh.

## Quick Start

1. **Map Ingress Domains:** Before starting, ensure your local machine (or WSL) can resolve the local `.localhost` domains.
   ```bash
   sudo bash -c 'echo "127.0.0.1 api.metacall.localhost router.metacall.localhost" >> /etc/hosts'
   ```

2. **Boot Cluster:** Create the kind cluster, install Cilium eBPF, and install NGINX Ingress Controller.
   ```bash
   make cluster-up
   ```

3. **Build & Deploy Operator:** Build the mesh API and router images, push them to the local registry, and install the Helm chart.
   ```bash
   make deploy
   ```

## Cluster Commands

| Command | Description |
|---|---|
| `make cluster-up` | Create kind cluster, local registry, install Cilium & Ingress |
| `make cluster-start` | Restart stopped cluster containers |
| `make cluster-stop` | Stop cluster containers (preserves state) |
| `make cluster-down` | Delete the kind cluster entirely |
| `make cluster-status` | Show nodes, pods, cluster info |

## Build & Deploy Commands

| Command | Description |
|---|---|
| `make build` | Go build all binaries |
| `make docker-build` | Build all Docker images |
| `make docker-push` | Push all images to local registry |
| `make deploy` | Build + push + Helm install + wait for rollout |
| `make helm-install` | Install/upgrade Helm chart only |
| `make clean` | Uninstall Helm release + delete CRDs/RBAC |

## Working with Functions

Functions can be deployed using the Ingress API URL (`api.metacall.localhost`). 

> [!IMPORTANT]
> **Cross-Pod Communication:** For two functions to communicate over the mesh, they **MUST** be deployed with the exact same `id`. The `id` acts as a deployment sandbox. Functions with different IDs cannot resolve each other's endpoints.

### Deploying a Package (via Zip)
```bash
zip -j -q /tmp/my-app.zip ./my/code/path/*
curl -sS -X POST -F "id=my-app" -F "file=@/tmp/my-app.zip" http://api.metacall.localhost/api/package/create
```

### Deploying from Git
```bash
curl -X POST http://api.metacall.localhost/api/repository/add \
  -H "Content-Type: application/json" \
  -d '{"url": "https://github.com/metacall/examples", "branch": "master", "suffix": "my-app"}'
```
*(Alternatively, use the `metacall-deploy` CLI)*

### Calling a Function
Once deployed, call the function via the router (`router.metacall.localhost`). 

> [!WARNING]
> You **must** send a valid JSON array as the HTTP body. If your function takes no arguments, you must explicitly send an empty array `-d '[]'`.

```bash
curl -X POST http://router.metacall.localhost/call/my-app/my_function \
  -H "Content-Type: application/json" \
  -d '["Hello World"]'
```

### Inspecting Cluster State
```bash
# Get all resources in the mesh
kubectl get all -n metacall-system
kubectl get all -n metacall-functions

# Inspect functions API
curl http://api.metacall.localhost/api/inspect

# inspect configMaps
kubectl get configmap <configmap-name> -n <namespace> -o yaml

# inspect pod working directory Commands
# 1- Print the current working directory (pwd):
kubectl exec -n metacall-functions -it <pod-name> -- pwd

# 2- List all files in the current directory:
kubectl exec -n metacall-functions -it <pod-name> -- ls

# 3- List all files including hidden files:
kubectl exec -n metacall-functions -it <pod-name> -- ls -la

# 4- Open an interactive shell inside the pod to explore:
kubectl exec -it <pod-name> -n metacall-functions -- sh
```

## Observability & Performance

This project uses **Cilium** as the CNI plugin with `kubeProxyReplacement=true`. This allows cross-pod function calls via `rpc_loader` to be routed directly at the **eBPF kernel level**, bypassing standard `iptables` overhead for maximum performance.

Install Prometheus, Grafana, Loki, and Grafana Alloy:
```bash
make monitoring-install
```

Open Grafana at <http://localhost:3000>:
```bash
make grafana-ui
```

The **Function Mesh Overview** dashboard contains metrics and a **Kubernetes Pod Logs** row. Use the `Log Namespace`, `Log Pod`, and `Log Container` selectors to view logs from any pod, or enter a regular expression in `Log Search` to filter messages. Logs are stored on a 10 GiB Loki PVC and retained for seven days.

### Distributed Tracing (OpenTelemetry + Tempo)
Function Mesh has built-in OpenTelemetry tracing for both the router and runtime pods. Traces are collected by Grafana Alloy and forwarded to **Grafana Tempo**, providing waterfall visualization of request phases (e.g. registry lookup, JSON parsing, function execution). 

To view traces:
1. Open Grafana and click the **Explore** icon in the sidebar (compass icon).
2. Select **Tempo** from the data source dropdown.
3. In the "Search" tab, select the `function-mesh-router` or `function-mesh-runtime` service name.
4. Click **Run query** to see a list of recent traces, then click on a Trace ID to view the waterfall chart.

By analyzing these spans, you can determine exactly how much time cross-pod requests spend in the native `rpc_loader` hop versus the router proxy and HTTP serialization layers.

Useful monitoring commands:

| Command | Description |
|---|---|
| `make prometheus-ui` | Open a local Prometheus port-forward on port 9090 |
| `make monitoring-reset-data` | Delete persisted Prometheus metrics |
| `make monitoring-reset-logs` | Delete persisted Loki logs |
| `make monitoring-clean` | Uninstall monitoring and delete metrics/log PVCs |

You can observe the live mesh network flows using Hubble. From your terminal, run:
```bash
cilium hubble ui
```
*This command automatically handles port-forwarding and opens the Hubble visualizer in your web browser.*

*Note: For maximum performance, L7 (HTTP-level) visibility is disabled by default to avoid Envoy proxy overhead. You can enable it by setting `cilium.hubble.l7Visibility: true` in `values.yaml`.*

## Tests

| Command | Description |
|---|---|
| `make test` | Run all tests |
| `make test-controller` | Controller tests only |
| `make test-router` | Router tests only |
| `make test-api` | API tests only |
