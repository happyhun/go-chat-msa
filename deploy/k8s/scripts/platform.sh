#!/usr/bin/env bash
set -euo pipefail
KUBECTL=(kubectl --context "${KUBE_CONTEXT:-kind-${KIND_CLUSTER:-go-chat}}")
K8S_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TEMP_DIR}"' EXIT
if "${KUBECTL[@]}" -n ingress-nginx get deployment ingress-nginx-controller >/dev/null 2>&1; then
  printf 'This cluster still uses ingress-nginx. Use a new KIND_CLUSTER and free local ports.\n' >&2
  exit 1
fi
"${KUBECTL[@]}" -n kube-system rollout status daemonset/kindnet --timeout=300s
"${KUBECTL[@]}" wait --for=condition=Ready nodes --all --timeout=300s
curl -fsSL https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml -o "${TEMP_DIR}/gateway-api.yaml"
printf '24d931f22abd8e40c973264319ead7cfa09d0fb7716b7ab1ee2ff174cb063a73  %s\n' "${TEMP_DIR}/gateway-api.yaml" | shasum -a 256 -c -
"${KUBECTL[@]}" apply --server-side -f "${TEMP_DIR}/gateway-api.yaml"
"${KUBECTL[@]}" wait --for=condition=Established crd/gateways.gateway.networking.k8s.io crd/httproutes.gateway.networking.k8s.io --timeout=120s
"${KUBECTL[@]}" apply -f "${K8S_DIR}/platform/traefik.yaml"
"${KUBECTL[@]}" -n gochat-system rollout status deployment/traefik --timeout=180s
