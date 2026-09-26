#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
REPO_ROOT="${ROOT}"
TIMEOUT="${K6_LOAD_TIMEOUT:-30m}"
FOLLOW_LOGS="${K6_FOLLOW_LOGS:-false}"
MAX_LOG_REQUESTS="${K6_MAX_LOG_REQUESTS:-4}"
LOG_PID=""
PROGRESS_ACTIVE=false
PROGRESS_PHASE=""
PROGRESS_KEY=""
PROGRESS_REASONS=""
PROGRESS_PRINTED_AT=-30
STARTED_AT=0

finish_progress() {
  if [[ "${PROGRESS_ACTIVE}" == true ]]; then
    printf '\n'
    PROGRESS_ACTIVE=false
  fi
}

show_progress() {
  local phase="$1" detail="$2"
  local elapsed=$((SECONDS - STARTED_AT)) line key
  printf -v line '%s | elapsed %02d:%02d | %s' "${phase}" "$((elapsed / 60))" "$((elapsed % 60))" "${detail}"
  key="${phase}|${detail}"
  if [[ -t 1 && "${FOLLOW_LOGS}" != true ]]; then
    if [[ "${phase}" != "${PROGRESS_PHASE}" ]]; then finish_progress; fi
    printf '\r\033[2K%s' "${line}"
    PROGRESS_ACTIVE=true
  elif [[ "${key}" != "${PROGRESS_KEY}" ]] || ((SECONDS - PROGRESS_PRINTED_AT >= 30)); then
    printf '[%s] %s\n' "$(date +%H:%M:%S)" "${line}"
    PROGRESS_PRINTED_AT=${SECONDS}
  fi
  PROGRESS_PHASE="${phase}"
  PROGRESS_KEY="${key}"
}

report_progress() {
  local pods="$1" expected="$2" condition="$3"
  local pod phase waiting started terminated exit_code
  local running=0 succeeded=0 failed=0 pending=0 detail="" reasons=""
  while IFS='|' read -r pod phase waiting started terminated exit_code; do
    [[ -n "${pod}" ]] || continue
    if [[ -n "${terminated}" ]]; then
      if [[ "${exit_code}" == 0 ]]; then
        succeeded=$((succeeded + 1))
      else
        failed=$((failed + 1))
        reasons+=" ${pod}:${terminated}(exit=${exit_code})"
      fi
    elif [[ "${phase}" == Failed ]]; then
      failed=$((failed + 1))
      reasons+=" ${pod}:Failed"
    elif [[ -n "${started}" ]]; then
      running=$((running + 1))
    else
      pending=$((pending + 1))
      reasons+=" ${pod}:${waiting:-${phase:-Pending}}"
    fi
  done <<< "${pods}"
  phase='Running'
  if ((pending > 0 || running + succeeded + failed < expected)); then
    phase='Preparing'
  elif ((succeeded > 0 || failed > 0)); then
    phase='Finishing'
  fi
  if [[ "${condition}" == *"Complete=True"* ]]; then phase='Complete'; fi
  if [[ "${condition}" == *"Failed=True"* ]]; then phase='Failed'; fi
  detail="workers: ${running} running, ${succeeded} succeeded, ${failed} failed / ${expected}"
  if [[ -n "${reasons}" ]]; then detail+=" |${reasons}"; fi
  if [[ "${reasons}" != "${PROGRESS_REASONS}" ]]; then finish_progress; fi
  PROGRESS_REASONS="${reasons}"
  show_progress "${phase}" "${detail}"
}

stop_log_follow() {
  if [[ -n "${LOG_PID}" ]]; then
    kill "${LOG_PID}" 2>/dev/null || true
    wait "${LOG_PID}" 2>/dev/null || true
    LOG_PID=""
  fi
}
trap 'finish_progress; stop_log_follow; restore_hpa; cleanup_charts' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log() {
  finish_progress
  printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"
}

load_args() {
  LOAD_ARGS=(--timeout "${TIMEOUT}")
  local key value
  for key in K6_WORKER_VUS K6_RAMP_DURATION K6_PLATEAU_DURATION K6_RAMP_DOWN_DURATION; do
    value="${!key:-}"
    [[ -n "${value}" ]] || continue
    if [[ "${key}" == K6_WORKER_VUS ]]; then
      [[ "${value}" =~ ^[1-9][0-9]*$ ]] || { printf 'Invalid %s\n' "${key}" >&2; return 1; }
    else
      [[ "${value}" =~ ^[1-9][0-9]*[smh]$ ]] || { printf 'Invalid %s\n' "${key}" >&2; return 1; }
    fi
    LOAD_ARGS+=(--set-string "env.${key}=${value}")
  done
}

HPA_RESET=false
restore_hpa() {
  if [[ "${HPA_RESET}" == true ]]; then
    "${KUBECTL[@]}" -n "${NAMESPACE}" apply -f "${STAGE}/hpa.yaml"
    HPA_RESET=false
  fi
}
reset_qa_hpa_start_state() {
  if [[ "${K8S_ENV}" != qa ]]; then return; fi
  render_group apps --show-only charts/websocket-service/templates/hpa.yaml > "${STAGE}/hpa.yaml"
  HPA_RESET=true
  log 'HPA test: resetting WebSocket to one Pod and waiting for readiness'
  "${KUBECTL[@]}" -n "${NAMESPACE}" delete hpa websocket-service --ignore-not-found=true --wait=true
  "${KUBECTL[@]}" -n "${NAMESPACE}" scale deployment/websocket-service --replicas=1
  "${KUBECTL[@]}" -n "${NAMESPACE}" rollout status deployment/websocket-service --timeout=120s
  restore_hpa
  "${KUBECTL[@]}" -n "${NAMESPACE}" wait --for=condition=AbleToScale hpa/websocket-service --timeout=120s
}

start_log_follow() {
  local remaining="$1"
  local pods parallelism pod started ready_count=0
  if [[ "${FOLLOW_LOGS}" != "true" ]]; then
    return
  fi
  if [[ -n "${LOG_PID}" ]]; then
    if kill -0 "${LOG_PID}" 2>/dev/null; then
      return
    fi
    wait "${LOG_PID}" 2>/dev/null || true
    LOG_PID=""
  fi
  parallelism="$("${KUBECTL[@]}" --request-timeout="${remaining}s" -n "${NAMESPACE}" \
    get "job/${JOB_NAME}" -o jsonpath='{.spec.parallelism}' 2>/dev/null)" || return 0
  pods="$("${KUBECTL[@]}" --request-timeout="${remaining}s" -n "${NAMESPACE}" \
    get pods -l "job-name=${JOB_NAME}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[?(@.name=="k6")].state.running.startedAt}{.status.containerStatuses[?(@.name=="k6")].state.terminated.finishedAt}{"\n"}{end}' \
    2>/dev/null)" || return 0
  while read -r pod started; do
    if [[ -z "${pod}" || -z "${started}" ]]; then
      return
    fi
    ready_count=$((ready_count + 1))
  done <<< "${pods}"
  if ((ready_count < ${parallelism:-1})); then return; fi
  log "k6 containers started; following logs"
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
  local message snapshot parallelism expected pods

  while ((SECONDS < end)); do
    remaining=$((end - SECONDS))
    if ((remaining > 5)); then remaining=5; fi
    if ! snapshot="$("${KUBECTL[@]}" --request-timeout="${remaining}s" -n "${NAMESPACE}" get "job/${JOB_NAME}" \
      -o jsonpath='{.spec.parallelism}{"|"}{.spec.completions}{"|"}{range .status.conditions[*]}{.type}={.status}{";"}{end}' 2>/dev/null)"; then
      show_progress 'Retrying' "Unable to fetch job/${JOB_NAME} status"
    else
      IFS='|' read -r parallelism expected status <<< "${snapshot}"
      if pods="$("${KUBECTL[@]}" --request-timeout="${remaining}s" -n "${NAMESPACE}" get pods -l "job-name=${JOB_NAME}" \
        -o jsonpath='{range .items[*]}{.metadata.name}{"|"}{.status.phase}{"|"}{.status.containerStatuses[?(@.name=="k6")].state.waiting.reason}{"|"}{.status.containerStatuses[?(@.name=="k6")].state.running.startedAt}{"|"}{.status.containerStatuses[?(@.name=="k6")].state.terminated.reason}{"|"}{.status.containerStatuses[?(@.name=="k6")].state.terminated.exitCode}{"\n"}{end}' 2>/dev/null)"; then
        report_progress "${pods}" "${expected:-${parallelism:-1}}" "${status}"
      else
        if [[ "${status}" == *"Complete=True"* ]]; then
          show_progress 'Complete' "job/${JOB_NAME} complete (worker details unavailable)"
        elif [[ "${status}" == *"Failed=True"* ]]; then
          show_progress 'Failed' "job/${JOB_NAME} failed (worker details unavailable)"
        else
          show_progress 'Retrying' 'Unable to fetch worker status'
        fi
      fi
      if [[ "${status}" == *"Complete=True"* ]]; then
        finish_progress
        return 0
      fi
      if [[ "${status}" == *"Failed=True"* ]]; then
        reason="$("${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" get "job/${JOB_NAME}" \
          -o jsonpath='{range .status.conditions[?(@.type=="Failed")]}{.reason}{end}' 2>/dev/null || true)"
        message="$("${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" get "job/${JOB_NAME}" \
          -o jsonpath='{range .status.conditions[?(@.type=="Failed")]}{.message}{end}' 2>/dev/null || true)"
        finish_progress
        printf 'job/%s failed: %s %s\n' "${JOB_NAME}" "${reason}" "${message}" >&2
        return 1
      fi
    fi
    remaining=$((end - SECONDS))
    if ((remaining > 0)); then start_log_follow "${remaining}"; fi
    delay=$((end - SECONDS))
    if ((delay > 1)); then delay=1; fi
    if ((delay > 0)); then sleep "${delay}"; fi
  done

  show_progress 'Timed out' "job/${JOB_NAME}: ${TIMEOUT}"
  finish_progress
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

print_results() {
  local pods pod summary
  pods="$("${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" get pods \
    -l "job-name=${JOB_NAME}" --sort-by=.metadata.name -o name)" || return 1
  [[ -n "${pods}" ]] || return 1
  while read -r pod; do
    printf '\n--- %s ---\n' "${pod}"
    if summary="$("${KUBECTL[@]}" --request-timeout=15s -n "${NAMESPACE}" logs "${pod}" -c k6 --tail=-1 | awk '
      /^ *█ (THRESHOLDS|TOTAL RESULTS)/ { found = 1 }
      /^(running|interrupted) / { if (found) finished = 1 }
      found && !finished && /^([[:space:]]|$)/ { print }
      END { if (!found) exit 1 }
    ')"; then
      printf '%s\n' "${summary}"
    else
      printf 'Final metrics unavailable; inspect with: kubectl --context %s -n %s logs %s -c k6\n' \
        "${KUBE_CONTEXT}" "${NAMESPACE}" "${pod}" >&2
    fi
  done <<< "${pods}"
}

dump_failure_context() {
  "${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" describe "job/${JOB_NAME}" || true
  "${KUBECTL[@]}" --request-timeout=5s -n "${NAMESPACE}" get pods -l "job-name=${JOB_NAME}" -o wide || true
}

main() {
  local timeout_seconds
  local deadline
  check_environment
  timeout_seconds="$(timeout_to_seconds "${TIMEOUT}")"

  cd "${REPO_ROOT}"

  log "${K8S_ENV}: preparing load test charts and configuration"
  prepare_charts
  load_args
  case "${K8S_ENV}" in
    dev|test) JOB_NAME=k6-c10k ;;
    qa) JOB_NAME=k6-hpa ;;
  esac
  log 'Removing previous load test resources and waiting for cleanup'
  "${HELM}" uninstall load --kube-context "${KUBE_CONTEXT}" -n "${NAMESPACE}" --ignore-not-found --wait --timeout 120s
  reset_qa_hpa_start_state
  log "${JOB_NAME}: creating load test Job"
  values_args load
  "${HELM}" install load "${STAGE}/load" --kube-context "${KUBE_CONTEXT}" -n "${NAMESPACE}" \
    "${VALUES[@]}" "${LOAD_ARGS[@]}"

  STARTED_AT=${SECONDS}
  deadline=$((STARTED_AT + timeout_seconds))

  log "${JOB_NAME}: monitoring test progress (timeout: ${TIMEOUT})"
  if ! wait_for_job_finished "${deadline}"; then
    stop_log_follow
    dump_failure_context
    print_results || log 'Unable to retrieve final load test metrics'
    return 1
  fi
  stop_log_follow
  log "${K8S_ENV}: load test complete; ${JOB_NAME} succeeded"
  print_results || log 'Unable to retrieve final load test metrics'
}

main "$@"
