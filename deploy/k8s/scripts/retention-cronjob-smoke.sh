#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="${NAMESPACE:-go-chat-test}"
CRONJOB_NAME="${CRONJOB_NAME:-retention-job}"
JOB_NAME="${JOB_NAME:-retention-job-smoke}"
TIMEOUT="${KUBECTL_TIMEOUT:-180s}"

dump_job() {
  echo "---- describe job/${JOB_NAME} ----" >&2
  kubectl -n "${NAMESPACE}" describe "job/${JOB_NAME}" >&2 || true

  echo "---- logs job/${JOB_NAME} ----" >&2
  kubectl -n "${NAMESPACE}" logs "job/${JOB_NAME}" --all-containers=true --tail=200 >&2 || true

  echo "---- pods for job/${JOB_NAME} ----" >&2
  kubectl -n "${NAMESPACE}" get pods -l "job-name=${JOB_NAME}" -o wide >&2 || true
}

echo "Checking cronjob/${CRONJOB_NAME} in namespace ${NAMESPACE}"
kubectl -n "${NAMESPACE}" get "cronjob/${CRONJOB_NAME}" >/dev/null

echo "Recreating job/${JOB_NAME} from cronjob/${CRONJOB_NAME}"
kubectl -n "${NAMESPACE}" delete "job/${JOB_NAME}" --ignore-not-found=true
kubectl -n "${NAMESPACE}" wait --for=delete "job/${JOB_NAME}" --timeout=60s >/dev/null 2>&1 || true
kubectl -n "${NAMESPACE}" create job "${JOB_NAME}" --from="cronjob/${CRONJOB_NAME}"

echo "Waiting for job/${JOB_NAME} completion"
if ! kubectl -n "${NAMESPACE}" wait --for=condition=complete "job/${JOB_NAME}" --timeout="${TIMEOUT}"; then
  dump_job
  exit 1
fi

echo "---- logs job/${JOB_NAME} ----"
kubectl -n "${NAMESPACE}" logs "job/${JOB_NAME}" --all-containers=true --tail=200 || true
echo "retention CronJob smoke completed"
