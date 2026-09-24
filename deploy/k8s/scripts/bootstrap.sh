#!/usr/bin/env bash
set -euo pipefail
KUBECTL=(kubectl --context "${KUBE_CONTEXT:-kind-${KIND_CLUSTER:-go-chat}}")
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K8S_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${K8S_DIR}/../.." && pwd)"
K8S_ENV="${K8S_ENV:-dev}"
NAMESPACE="go-chat-${K8S_ENV}"
OVERLAY_DIR="${K8S_DIR}/overlays/${K8S_ENV}"
TIMEOUT="${KUBECTL_TIMEOUT:-300s}"
IMAGE_TAG="${IMAGE_TAG:-$(bash "${SCRIPT_DIR}/image-tag.sh")}"

log() { printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
wait_rollout() {
  local name
  for name in "$@"; do
    "${KUBECTL[@]}" -n "${NAMESPACE}" rollout status "deployment/${name}" --timeout="${TIMEOUT}"
  done
}
apply_apps() {
  if [[ ! "${IMAGE_TAG}" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]]; then
    printf 'Invalid IMAGE_TAG\n' >&2
    return 1
  fi
  "${KUBECTL[@]}" kustomize "${OVERLAY_DIR}/apps" \
    | sed -E "s|(image: go-chat-msa/[a-z-]+):build-required$|\1:${IMAGE_TAG}|" \
    | "${KUBECTL[@]}" apply -f -
}
main() {
  case "${K8S_ENV}" in dev|test|qa) ;; *) printf 'Invalid K8S_ENV\n' >&2; exit 1 ;; esac
  cd "${REPO_ROOT}"
  "${KUBECTL[@]}" apply -f "${OVERLAY_DIR}/namespace.yaml"
  log 'Applying ephemeral foundation'
  "${KUBECTL[@]}" apply -k "${OVERLAY_DIR}/foundation"
  wait_rollout postgres mongo redis
  "${KUBECTL[@]}" -n "${NAMESPACE}" rollout status statefulset/nats --timeout="${TIMEOUT}"
  log 'Applying observability and hashed configuration'
  if [[ "${K8S_ENV}" == qa ]]; then
    "${KUBECTL[@]}" apply -f "${OVERLAY_DIR}/observability/prometheus-adapter-auth-reader.yaml"
  fi
  "${KUBECTL[@]}" apply -k "${OVERLAY_DIR}/observability"
  wait_rollout kube-state-metrics prometheus loki tempo pyroscope alloy grafana
  if [[ "${K8S_ENV}" == qa ]]; then wait_rollout prometheus-adapter; fi
  log 'Running migrations'
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete job postgres-migrate mongo-migrate --ignore-not-found=true --wait=true
  "${KUBECTL[@]}" apply -k "${OVERLAY_DIR}/migrations"
  local job
  for job in postgres-migrate mongo-migrate; do
    if ! "${KUBECTL[@]}" -n "${NAMESPACE}" wait --for=condition=complete "job/${job}" --timeout="${TIMEOUT}"; then
      "${KUBECTL[@]}" -n "${NAMESPACE}" logs "job/${job}" --all-containers=true --tail=100
      return 1
    fi
  done
  log "Applying application image ${IMAGE_TAG}"
  apply_apps
  wait_rollout user-service chat-service websocket-service api-gateway frontend swagger-ui
  "${KUBECTL[@]}" -n "${NAMESPACE}" wait --for=condition=Programmed gateway/gochat --timeout="${TIMEOUT}"
  log "Ready: http://${K8S_ENV}.gochat.localhost:30080/"
  log "Grafana login: admin / dev_grafana_password"
}
main "$@"
