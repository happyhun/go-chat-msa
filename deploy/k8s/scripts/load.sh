#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K8S_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${K8S_DIR}/../.." && pwd)"

K8S_ENV="${K8S_ENV:-dev}"
NAMESPACE="${NAMESPACE:-go-chat-${K8S_ENV}}"
OVERLAY_DIR="${K8S_DIR}/overlays/${K8S_ENV}/load"
JOB_NAME="${K6_JOB_NAME:-k6-c10k}"
TIMEOUT="${K6_LOAD_TIMEOUT:-30m}"
FOLLOW_LOGS="${K6_FOLLOW_LOGS:-true}"
MAX_LOG_REQUESTS="${K6_MAX_LOG_REQUESTS:-4}"

log() {
  printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"
}

literal_arg() {
  printf -- '--from-literal=%s=%s' "$1" "$2"
}

create_script_configmap() {
  log "creating configmap/k6-load-scripts"
  kubectl -n "${NAMESPACE}" create configmap k6-load-scripts \
    "--from-file=${REPO_ROOT}/test/load" \
    --dry-run=client -o yaml | kubectl apply -f -
}

create_env_configmap() {
  local args=(
    "$(literal_arg API_HOST "api-gateway")"
    "$(literal_arg API_PORT "8080")"
    "$(literal_arg WS_HOST "ws-gateway")"
    "$(literal_arg WS_PORT "8088")"
  )

  log "creating configmap/k6-load-env"
  kubectl -n "${NAMESPACE}" create configmap k6-load-env \
    "${args[@]}" \
    --dry-run=client -o yaml | kubectl apply -f -
}

delete_previous_job() {
  log "deleting previous job/${JOB_NAME}"
  kubectl -n "${NAMESPACE}" delete "job/${JOB_NAME}" --ignore-not-found=true
  kubectl -n "${NAMESPACE}" wait --for=delete "job/${JOB_NAME}" --timeout=60s >/dev/null 2>&1 || true
}

reset_qa_hpa_start_state() {
  if [[ "${K8S_ENV}" != "qa" || "${JOB_NAME}" != "k6-hpa" ]]; then
    return
  fi

  log "resetting websocket-service to 1 replica before HPA test"
  kubectl -n "${NAMESPACE}" delete hpa websocket-service --ignore-not-found=true
  kubectl -n "${NAMESPACE}" wait --for=delete hpa/websocket-service --timeout=60s >/dev/null 2>&1 || true
  kubectl -n "${NAMESPACE}" scale deployment/websocket-service --replicas=1
  kubectl -n "${NAMESPACE}" rollout status deployment/websocket-service --timeout=120s
  kubectl -n "${NAMESPACE}" wait --for=condition=Available deployment/websocket-service --timeout=120s

  log "reapplying websocket-service HPA"
  kubectl -n "${NAMESPACE}" apply -f "${K8S_DIR}/overlays/qa/apps/websocket-service-hpa.yaml"
  kubectl -n "${NAMESPACE}" wait --for=condition=AbleToScale hpa/websocket-service --timeout=60s >/dev/null 2>&1 || true
}

wait_for_pods() {
  local i
  for i in {1..60}; do
    if kubectl -n "${NAMESPACE}" get pod -l "job-name=${JOB_NAME}" -o name | grep -q .; then
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_job_finished() {
  local end
  local status
  local reason
  local message

  end=$((SECONDS + $(timeout_to_seconds "${TIMEOUT}")))
  while ((SECONDS < end)); do
    status="$(kubectl -n "${NAMESPACE}" get "job/${JOB_NAME}" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status}{";"}{end}' 2>/dev/null || true)"
    if [[ "${status}" == *"Complete=True"* ]]; then
      return 0
    fi
    if [[ "${status}" == *"Failed=True"* ]]; then
      reason="$(kubectl -n "${NAMESPACE}" get "job/${JOB_NAME}" \
        -o jsonpath='{range .status.conditions[?(@.type=="Failed")]}{.reason}{end}' 2>/dev/null || true)"
      message="$(kubectl -n "${NAMESPACE}" get "job/${JOB_NAME}" \
        -o jsonpath='{range .status.conditions[?(@.type=="Failed")]}{.message}{end}' 2>/dev/null || true)"
      printf 'job/%s failed: %s %s\n' "${JOB_NAME}" "${reason}" "${message}" >&2
      return 1
    fi
    sleep 5
  done

  printf 'timed out waiting for job/%s after %s\n' "${JOB_NAME}" "${TIMEOUT}" >&2
  return 1
}

timeout_to_seconds() {
  local value="$1"
  case "${value}" in
    *s) printf '%s\n' "${value%s}" ;;
    *m) printf '%s\n' "$(( ${value%m} * 60 ))" ;;
    *h) printf '%s\n' "$(( ${value%h} * 3600 ))" ;;
    *) printf '%s\n' "${value}" ;;
  esac
}

dump_failure_context() {
  kubectl -n "${NAMESPACE}" describe "job/${JOB_NAME}" || true
  kubectl -n "${NAMESPACE}" get pods -l "job-name=${JOB_NAME}" -o wide || true
  kubectl -n "${NAMESPACE}" logs -l "job-name=${JOB_NAME}" \
    --all-containers=true \
    --prefix=true \
    --tail=200 \
    "--max-log-requests=${MAX_LOG_REQUESTS}" || true
}

main() {
  cd "${REPO_ROOT}"

  if [[ ! -d "${OVERLAY_DIR}" ]]; then
    printf 'unknown K8S_ENV=%s: load overlay not found: %s\n' "${K8S_ENV}" "${OVERLAY_DIR}" >&2
    exit 1
  fi

  create_script_configmap
  create_env_configmap
  delete_previous_job
  reset_qa_hpa_start_state

  log "starting job/${JOB_NAME}"
  kubectl apply -k "${OVERLAY_DIR}"

  if [[ "${FOLLOW_LOGS}" == "true" ]]; then
    wait_for_pods || true
    kubectl -n "${NAMESPACE}" wait \
      --for=condition=Ready pod \
      -l "job-name=${JOB_NAME}" \
      --timeout=60s >/dev/null 2>&1 || true
    kubectl -n "${NAMESPACE}" logs -l "job-name=${JOB_NAME}" \
      --follow \
      --all-containers=true \
      --prefix=true \
      "--max-log-requests=${MAX_LOG_REQUESTS}" || true
  fi

  log "waiting for job/${JOB_NAME}"
  if ! wait_for_job_finished; then
    dump_failure_context
    return 1
  fi
}

main "$@"
