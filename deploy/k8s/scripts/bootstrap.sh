#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K8S_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${K8S_DIR}/../.." && pwd)"

K8S_ENV="${K8S_ENV:-dev}"
OVERLAY_DIR="${K8S_DIR}/overlays/${K8S_ENV}"
if [[ -z "${NAMESPACE:-}" ]]; then
  NAMESPACE="go-chat-${K8S_ENV}"
fi
K8S_HOST="${K8S_HOST:-${K8S_ENV}.gochat.localhost}"
TIMEOUT="${KUBECTL_TIMEOUT:-180s}"
TMP_FILES=()

cleanup_tmp_files() {
  local file
  if ((${#TMP_FILES[@]})); then
    for file in "${TMP_FILES[@]}"; do
      [[ -f "${file}" ]] && rm -f "${file}"
    done
  fi
}
trap cleanup_tmp_files EXIT

log() {
  printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"
}

apply_kustomize() {
  local path="$1"
  log "kubectl apply -k ${path}"
  kubectl apply -k "${path}"
}

apply_file_if_exists() {
  local path="$1"
  if [[ -f "${path}" ]]; then
    log "kubectl apply -f ${path}"
    kubectl apply -f "${path}"
  fi
}

ensure_namespace() {
  log "ensuring namespace/${NAMESPACE}"
  kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
}

wait_rollout() {
  local deployment
  for deployment in "$@"; do
    log "waiting for deployment/${deployment}"
    kubectl -n "${NAMESPACE}" rollout status "deployment/${deployment}" --timeout="${TIMEOUT}"
  done
}

wait_rollout_if_exists() {
  local deployments=()
  local deployment
  for deployment in "$@"; do
    if kubectl -n "${NAMESPACE}" get "deployment/${deployment}" >/dev/null 2>&1; then
      deployments+=("${deployment}")
    fi
  done
  if ((${#deployments[@]})); then
    wait_rollout "${deployments[@]}"
  fi
}

create_configmap_from_file() {
  local name="$1"
  local key="$2"
  local file="$3"

  log "creating configmap/${name} from ${file}"
  kubectl -n "${NAMESPACE}" create configmap "${name}" \
    "--from-file=${key}=${file}" \
    --dry-run=client -o yaml | kubectl apply -f -
}

create_configmap_from_dir() {
  local name="$1"
  local dir="$2"

  log "creating configmap/${name} from ${dir}"
  kubectl -n "${NAMESPACE}" create configmap "${name}" \
    "--from-file=${dir}" \
    --dry-run=client -o yaml | kubectl apply -f -
}

create_alloy_configmap() {
  local rendered
  rendered="$(mktemp)"
  TMP_FILES+=("${rendered}")
  sed "s/__GOCHAT_NAMESPACE__/${NAMESPACE}/g" "${K8S_DIR}/base/observability/config/alloy/config.alloy" > "${rendered}"
  create_configmap_from_file alloy-config config.alloy "${rendered}"
}

create_observability_configmaps() {
  create_alloy_configmap
  create_configmap_from_file prometheus-config config.yaml "${REPO_ROOT}/observability/prometheus/config.yaml"
  create_configmap_from_file loki-config config.yaml "${REPO_ROOT}/observability/loki/config.yaml"
  create_configmap_from_file tempo-config config.yaml "${REPO_ROOT}/observability/tempo/config.yaml"
  create_configmap_from_file pyroscope-config config.yaml "${REPO_ROOT}/observability/pyroscope/config.yaml"
  create_configmap_from_dir grafana-datasources "${REPO_ROOT}/observability/grafana/provisioning/datasources"
  create_configmap_from_dir grafana-dashboards "${REPO_ROOT}/observability/grafana/provisioning/dashboards"
}

create_migration_configmaps() {
  create_configmap_from_dir postgres-migrations "${REPO_ROOT}/db/migrations/postgres"
  create_configmap_from_dir mongo-migrations "${REPO_ROOT}/db/migrations/mongo"
}

create_app_configmaps() {
  create_configmap_from_file openapi-spec openapi.yaml "${REPO_ROOT}/api/openapi/openapi.yaml"
}

create_load_test_configmaps() {
  if [[ -d "${OVERLAY_DIR}/load" ]]; then
    create_configmap_from_dir k6-load-scripts "${REPO_ROOT}/test/load"
  fi
}

delete_previous_migration_jobs() {
  log "deleting previous migration jobs"
  kubectl -n "${NAMESPACE}" delete job postgres-migrate mongo-migrate --ignore-not-found=true
  kubectl -n "${NAMESPACE}" wait --for=delete job/postgres-migrate --timeout=60s 2>/dev/null || true
  kubectl -n "${NAMESPACE}" wait --for=delete job/mongo-migrate --timeout=60s 2>/dev/null || true
}

wait_job_complete() {
  local job="$1"

  log "waiting for job/${job}"
  if ! kubectl -n "${NAMESPACE}" wait --for=condition=complete "job/${job}" --timeout="${TIMEOUT}"; then
    kubectl -n "${NAMESPACE}" describe "job/${job}" || true
    kubectl -n "${NAMESPACE}" logs "job/${job}" --all-containers=true --tail=200 || true
    return 1
  fi
}

restart_backend_apps() {
  log "restarting core backend deployments after image/config apply"
  kubectl -n "${NAMESPACE}" rollout restart \
    deployment/user-service \
    deployment/chat-service
  wait_rollout user-service chat-service

  log "restarting websocket deployment after core backend rollout"
  kubectl -n "${NAMESPACE}" rollout restart deployment/websocket-service
  wait_rollout websocket-service
}

restart_edge_apps() {
  log "restarting edge deployments after backend rollout"
  kubectl -n "${NAMESPACE}" rollout restart \
    deployment/api-gateway \
    deployment/ws-gateway \
    deployment/frontend \
    deployment/swagger-ui
}

main() {
  cd "${REPO_ROOT}"

  if [[ ! -d "${OVERLAY_DIR}" ]]; then
    printf 'unknown K8S_ENV=%s: overlay not found: %s\n' "${K8S_ENV}" "${OVERLAY_DIR}" >&2
    exit 1
  fi

  ensure_namespace
  apply_kustomize "${OVERLAY_DIR}/foundation"
  wait_rollout postgres mongo redis

  create_observability_configmaps
  apply_file_if_exists "${OVERLAY_DIR}/observability/prometheus-adapter-auth-reader.yaml"
  apply_kustomize "${OVERLAY_DIR}/observability"
  wait_rollout_if_exists kube-state-metrics prometheus loki tempo pyroscope alloy grafana prometheus-adapter

  create_migration_configmaps
  delete_previous_migration_jobs
  apply_kustomize "${OVERLAY_DIR}/migrations"
  wait_job_complete postgres-migrate
  wait_job_complete mongo-migrate

  create_app_configmaps
  create_load_test_configmaps
  apply_kustomize "${OVERLAY_DIR}/apps"
  restart_backend_apps
  restart_edge_apps
  wait_rollout api-gateway ws-gateway frontend swagger-ui

  log "${K8S_ENV} Kubernetes bootstrap completed"
  log "Frontend: http://${K8S_HOST}:30080/"
  log "OpenAPI: http://${K8S_HOST}:30080/docs/"
  log "Grafana: http://${K8S_HOST}:30080/grafana/"
}

main "$@"
