#!/usr/bin/env bash
set -euo pipefail

KUBECTL=(kubectl --context "${KUBE_CONTEXT:-kind-${KIND_CLUSTER:-go-chat}}")

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
LOG_PID=""

stop_log_follow() {
  if [[ -n "${LOG_PID}" ]]; then
    kill "${LOG_PID}" 2>/dev/null || true
    wait "${LOG_PID}" 2>/dev/null || true
    LOG_PID=""
  fi
}
trap stop_log_follow EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log() {
  printf '\n[%s] %s\n' "$(date +%H:%M:%S)" "$*"
}

render_load() {
  local substitutions=() key value
  for key in K6_WORKER_VUS K6_RAMP_DURATION K6_PLATEAU_DURATION K6_RAMP_DOWN_DURATION; do
    value="${!key:-}"
    if [[ -n "${value}" ]]; then
      if [[ "${key}" == K6_WORKER_VUS ]]; then
        [[ "${value}" =~ ^[1-9][0-9]*$ ]] || { printf 'Invalid %s\n' "${key}" >&2; return 1; }
      else
        [[ "${value}" =~ ^[1-9][0-9]*[smh]$ ]] || { printf 'Invalid %s\n' "${key}" >&2; return 1; }
      fi
    fi
    substitutions+=(-e "s/^  ${key}: .*/  ${key}: \"${value}\"/")
  done
  "${KUBECTL[@]}" kustomize "${OVERLAY_DIR}" \
    | sed "${substitutions[@]}"
}

delete_previous_job() {
  log "deleting previous job/${JOB_NAME}"
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete "job/${JOB_NAME}" --ignore-not-found=true
  "${KUBECTL[@]}" -n "${NAMESPACE}" wait --for=delete "job/${JOB_NAME}" --timeout=60s >/dev/null 2>&1 || true
}

reset_qa_hpa_start_state() {
  if [[ "${K8S_ENV}" != "qa" || "${JOB_NAME}" != "k6-hpa" ]]; then
    return
  fi

  log "resetting websocket-service to 1 replica before HPA test"
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete hpa websocket-service --ignore-not-found=true
  "${KUBECTL[@]}" -n "${NAMESPACE}" wait --for=delete hpa/websocket-service --timeout=60s >/dev/null 2>&1 || true
  "${KUBECTL[@]}" -n "${NAMESPACE}" scale deployment/websocket-service --replicas=1
  "${KUBECTL[@]}" -n "${NAMESPACE}" rollout status deployment/websocket-service --timeout=120s
  "${KUBECTL[@]}" -n "${NAMESPACE}" wait --for=condition=Available deployment/websocket-service --timeout=120s

  log "reapplying websocket-service HPA"
  "${KUBECTL[@]}" -n "${NAMESPACE}" apply -f "${K8S_DIR}/overlays/qa/apps/websocket-service-hpa.yaml"
  "${KUBECTL[@]}" -n "${NAMESPACE}" wait --for=condition=AbleToScale hpa/websocket-service --timeout=60s >/dev/null 2>&1 || true
}

start_log_follow() {
  local remaining="$1"
  local pods
  if [[ "${FOLLOW_LOGS}" != "true" || -n "${LOG_PID}" ]]; then
    return
  fi
  pods="$("${KUBECTL[@]}" --request-timeout="${remaining}s" -n "${NAMESPACE}" \
    get pods -l "job-name=${JOB_NAME}" -o name 2>/dev/null)" || return 0
  if [[ -z "${pods}" ]]; then
    return
  fi
  "${KUBECTL[@]}" -n "${NAMESPACE}" logs -l "job-name=${JOB_NAME}" \
    --follow --all-containers=true --prefix=true --tail=-1 \
    "--pod-running-timeout=${remaining}s" \
    "--max-log-requests=${MAX_LOG_REQUESTS}" &
  LOG_PID=$!
}

wait_for_job_finished() {
  local end="$1"
  local remaining
  local delay
  local status
  local reason
  local message

  while ((SECONDS < end)); do
    remaining=$((end - SECONDS))
    start_log_follow "${remaining}"
    remaining=$((end - SECONDS))
    if ((remaining <= 0)); then
      break
    fi
    status="$("${KUBECTL[@]}" --request-timeout="${remaining}s" -n "${NAMESPACE}" get "job/${JOB_NAME}" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status}{";"}{end}' 2>/dev/null || true)"
    if [[ "${status}" == *"Complete=True"* ]]; then
      return 0
    fi
    if [[ "${status}" == *"Failed=True"* ]]; then
      reason="$("${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" get "job/${JOB_NAME}" \
        -o jsonpath='{range .status.conditions[?(@.type=="Failed")]}{.reason}{end}' 2>/dev/null || true)"
      message="$("${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" get "job/${JOB_NAME}" \
        -o jsonpath='{range .status.conditions[?(@.type=="Failed")]}{.message}{end}' 2>/dev/null || true)"
      printf 'job/%s failed: %s %s\n' "${JOB_NAME}" "${reason}" "${message}" >&2
      return 1
    fi
    delay=$((end - SECONDS))
    if ((delay > 5)); then delay=5; fi
    if ((delay > 0)); then sleep "${delay}"; fi
  done

  printf 'timed out waiting for job/%s after %s\n' "${JOB_NAME}" "${TIMEOUT}" >&2
  return 1
}

timeout_to_seconds() {
  local value="$1"
  if [[ ! "${value}" =~ ^([0-9]+)([smh]?)$ ]]; then
    printf 'invalid K6_LOAD_TIMEOUT: %s (expected a positive integer with optional s, m or h)\n' "${value}" >&2
    return 1
  fi
  local seconds=$((10#${BASH_REMATCH[1]}))
  case "${BASH_REMATCH[2]}" in
    m) seconds=$((seconds * 60)) ;;
    h) seconds=$((seconds * 3600)) ;;
  esac
  if ((seconds <= 0)); then
    printf 'K6_LOAD_TIMEOUT must be positive\n' >&2
    return 1
  fi
  printf '%s\n' "${seconds}"
}

dump_failure_context() {
  "${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" describe "job/${JOB_NAME}" || true
  "${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" get pods -l "job-name=${JOB_NAME}" -o wide || true
  "${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" logs -l "job-name=${JOB_NAME}" \
    --all-containers=true \
    --prefix=true \
    --pod-running-timeout=5s --tail=200 \
    "--max-log-requests=${MAX_LOG_REQUESTS}" || true
}

main() {
  local timeout_seconds
  local deadline
  timeout_seconds="$(timeout_to_seconds "${TIMEOUT}")"

  cd "${REPO_ROOT}"

  if [[ ! -d "${OVERLAY_DIR}" ]]; then
    printf 'unknown K8S_ENV=%s: load overlay not found: %s\n' "${K8S_ENV}" "${OVERLAY_DIR}" >&2
    exit 1
  fi

  local manifest
  manifest="$(render_load)"
  delete_previous_job
  reset_qa_hpa_start_state

  log "starting job/${JOB_NAME}"
  printf '%s\n' "${manifest}" | "${KUBECTL[@]}" apply -f -

  deadline=$((SECONDS + timeout_seconds))

  log "waiting for job/${JOB_NAME}"
  if ! wait_for_job_finished "${deadline}"; then
    stop_log_follow
    dump_failure_context
    return 1
  fi
}

main "$@"
