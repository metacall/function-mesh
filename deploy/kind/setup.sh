#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-metacall-mesh}"
REGISTRY_NAME="${REGISTRY_NAME:-kind-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5000}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if docker inspect "${REGISTRY_NAME}" >/dev/null 2>&1; then
  docker start "${REGISTRY_NAME}" >/dev/null
else
  docker run -d --restart=always -p "${REGISTRY_PORT}:5000" --name "${REGISTRY_NAME}" registry:2
fi

if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  echo "Using existing kind cluster ${CLUSTER_NAME}"
  echo "If this cluster was created before this config changed, recreate it with: kind delete cluster --name ${CLUSTER_NAME}"
else
  kind create cluster --config="${SCRIPT_DIR}/kind-config.yaml" --name="${CLUSTER_NAME}"
fi

docker network connect kind "${REGISTRY_NAME}" 2>/dev/null || true
cilium install --version 1.16.0 \
  --set kubeProxyReplacement=true \
  --set k8sServiceHost="${CLUSTER_NAME}-control-plane" \
  --set k8sServicePort=6443
cilium status --wait
cilium hubble enable --ui
kubectl create namespace metacall-functions --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace metacall-system --dry-run=client -o yaml | kubectl apply -f -
echo "Cluster ready. Registry at localhost:5000"