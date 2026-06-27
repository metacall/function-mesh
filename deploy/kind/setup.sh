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


CILIUM_VERSION="${CILIUM_VERSION:-1.16.0}"
CILIUM_ENVOY_TAG="${CILIUM_ENVOY_TAG:-v1.29.7-39a2a56bbd5b3a591f69dbca51d3e30ef97e0e51}"
HUBBLE_UI_VERSION="${HUBBLE_UI_VERSION:-0.13.1}"
CILIUM_IMAGES=(
  "quay.io/cilium/cilium:v${CILIUM_VERSION}"
  "quay.io/cilium/operator-generic:v${CILIUM_VERSION}"
  "quay.io/cilium/cilium-envoy:${CILIUM_ENVOY_TAG}"
  "quay.io/cilium/hubble-relay:v${CILIUM_VERSION}"
  "quay.io/cilium/hubble-ui:v${HUBBLE_UI_VERSION}"
  "quay.io/cilium/hubble-ui-backend:v${HUBBLE_UI_VERSION}"
)
echo "Pre-pulling Cilium/Hubble images..."
for img in "${CILIUM_IMAGES[@]}"; do
  docker pull "${img}" || true
done
echo "Loading images into kind cluster..."
kind load docker-image --name "${CLUSTER_NAME}" "${CILIUM_IMAGES[@]}"

if cilium status --wait=false >/dev/null 2>&1; then
  echo "Cilium already installed, skipping install"
else
  cilium install --version "${CILIUM_VERSION}" \
    --set kubeProxyReplacement=true \
    --set k8sServiceHost="${CLUSTER_NAME}-control-plane" \
    --set k8sServicePort=6443
fi
cilium status --wait
cilium hubble enable --ui
cilium status --wait
echo "Cluster ready. Registry at localhost:5000"