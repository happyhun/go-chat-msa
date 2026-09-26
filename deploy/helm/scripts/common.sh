#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
HELM="${HELM:-helm}"
HELM_DIR="${ROOT}/deploy/helm"
K8S_ENV="${K8S_ENV:-dev}"
NAMESPACE="go-chat-${K8S_ENV}"
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-${KIND_CLUSTER:-go-chat}}"
KUBECTL=(kubectl --context "${KUBE_CONTEXT}")
TIMEOUT="${KUBECTL_TIMEOUT:-300s}"
STAGE=""

check_environment() {
  case "${K8S_ENV}" in
    dev|test|qa) ;;
    *)
      printf 'Invalid K8S_ENV: %s\n' "${K8S_ENV}" >&2
      exit 1
      ;;
  esac
}

check_helm() {
  local version
  if ! command -v "${HELM}" >/dev/null; then
    printf 'Required tool not found: %s. Install it and try again.\n' "${HELM}" >&2
    return 1
  fi
  version="$("${HELM}" version --template '{{.Version}}')"
  # https://helm.sh/docs/topics/version_skew/ — kind-local.yaml uses Kubernetes 1.37.
  if [[ "${version}" =~ ^v([0-9]+)\.([0-9]+)\.[0-9]+(\+[0-9A-Za-z.-]+)?$ ]] &&
     ((10#${BASH_REMATCH[1]} > 4 || (10#${BASH_REMATCH[1]} == 4 && 10#${BASH_REMATCH[2]} >= 3))); then
    return 0
  fi
  printf 'Helm 4.3.0 or newer is required (found %s). Update Helm and try again.\n' "${version}" >&2
  return 1
}

progress() {
  printf '[deploy] %s\n' "$*"
}

chart_dependencies_ready() {
  local chart="$1"
  "${HELM}" dependency list "${chart}" | awk '
    NR > 1 && NF && $NF != "ok" { failed = 1 }
    END { exit failed }
  '
}

prepare_dependencies() {
  local force=false repos_ready=false group chart stamp checksum
  if [[ "${1:-}" == --force ]]; then
    force=true
    shift
  fi
  if (($# == 0)); then
    set -- infra apps observability platform
  fi
  for group in "$@"; do
    chart="${HELM_DIR}/charts/${group}"
    stamp="${chart}/charts/.dependencies.sha256"
    checksum="$(shasum -a 256 "${chart}/Chart.yaml" "${chart}/Chart.lock")"
    if [[ "${force}" == false && -f "${stamp}" && "$(cat "${stamp}")" == "${checksum}" ]] &&
       chart_dependencies_ready "${chart}"; then
      continue
    fi
    if [[ "${repos_ready}" == false ]]; then
      "${HELM}" repo add gochat-nats https://nats-io.github.io/k8s/helm/charts/
      "${HELM}" repo add gochat-traefik https://traefik.github.io/charts
      "${HELM}" repo add gochat-grafana https://grafana.github.io/helm-charts
      "${HELM}" repo add gochat-grafana-community https://grafana-community.github.io/helm-charts
      "${HELM}" repo add gochat-prometheus https://prometheus-community.github.io/helm-charts
      repos_ready=true
    fi
    rm -f "${stamp}"
    progress "${group}: downloading chart dependencies"
    "${HELM}" dependency build "${chart}"
    printf '%s\n' "${checksum}" > "${stamp}"
  done
}

copy_files() {
  local target="$1"
  shift
  mkdir -p "${STAGE}/${target}"
  cp "$@" "${STAGE}/${target}/"
}

prepare_charts() {
  local group dependencies name version repository dependency_status
  check_helm
  prepare_dependencies infra apps observability "$@"
  STAGE="$(mktemp -d)"
  cp -R "${HELM_DIR}/charts/." "${STAGE}/"
  copy_files services/swagger-ui/files/openapi-spec "${ROOT}/api/openapi/openapi.yaml"
  mkdir -p "${STAGE}/observability/files"
  cp -R "${ROOT}/observability/." "${STAGE}/observability/files/"
  copy_files migrations/files/postgres-migrations "${ROOT}/db/migrations/postgres/"*.sql
  copy_files migrations/files/mongo-migrations "${ROOT}/db/migrations/mongo/"*.json
  copy_files load/files/k6-load-scripts "${ROOT}/test/load/"*.js
  for group in infra apps observability; do
    dependencies="$("${HELM}" dependency list "${STAGE}/${group}")"
    while read -r name version repository dependency_status; do
      [[ "${repository}" == file://* ]] || continue
      "${HELM}" package "${STAGE}/${group}/${repository#file://}" --destination "${STAGE}/${group}/charts" >/dev/null
    done <<< "${dependencies}"
    if ! chart_dependencies_ready "${STAGE}/${group}"; then
      printf 'Invalid chart dependencies for %s: retry with make helm-deps\n' "${group}" >&2
      exit 1
    fi
  done
}
cleanup_charts() {
  if [[ -n "${STAGE}" ]]; then
    rm -rf "${STAGE}"
  fi
}

values_args() {
  local group="$1"
  VALUES=(
    -f "${HELM_DIR}/values/common/${group}.yaml"
    -f "${HELM_DIR}/values/${K8S_ENV}/${group}.yaml"
  )
  if [[ "${group}" == observability ]]; then
    VALUES+=(
      --set-file "loki.loki.config=${ROOT}/observability/loki/config.yaml"
      --set-file "tempo.config=${ROOT}/observability/tempo/config.yaml"
      --set-file "pyroscope.pyroscope.config=${ROOT}/observability/pyroscope/config.yaml"
      --set "alloy.rbac.namespaces[0]=${NAMESPACE}"
    )
    local component annotation checksum
    for component in prometheus grafana alloy; do
      checksum="$(find "${ROOT}/observability/${component}" -type f -exec shasum -a 256 {} + | sort | shasum -a 256 | cut -d ' ' -f 1)"
      annotation="${component}.podAnnotations.checksum/config"
      if [[ "${component}" == prometheus ]]; then annotation="prometheus.server.podAnnotations.checksum/config"; fi
      if [[ "${component}" == alloy ]]; then annotation="alloy.controller.podAnnotations.checksum/config"; fi
      VALUES+=(--set-string "${annotation}=${checksum}")
    done
  fi
}
upgrade_group() {
  local group="$1"
  shift
  values_args "${group}"
  "${HELM}" upgrade --install "${group}" "${STAGE}/${group}" \
    --kube-context "${KUBE_CONTEXT}" -n "${NAMESPACE}" --create-namespace \
    --reset-values --wait=legacy --timeout "${TIMEOUT}" "${VALUES[@]}" "$@"
}
render_group() {
  local group="$1"
  shift
  values_args "${group}"
  "${HELM}" template "${group}" "${STAGE}/${group}" -n "${NAMESPACE}" "${VALUES[@]}" "$@"
}
