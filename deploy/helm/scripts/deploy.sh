#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
if [[ $# != 1 || ( "$1" != up && "$1" != down && "$1" != platform ) ]]; then
  printf 'Usage: deploy.sh up|down|platform\n' >&2
  exit 1
fi
check_environment
check_helm
trap cleanup_charts EXIT

platform() {
  progress 'Platform: preparing the Traefik chart'
  prepare_dependencies platform
  STAGE="$(mktemp -d)"
  if "${KUBECTL[@]}" -n ingress-nginx get deployment ingress-nginx-controller >/dev/null 2>&1; then
    printf 'This cluster still uses ingress-nginx. Use a new KIND_CLUSTER and free local ports.\n' >&2
    exit 1
  fi
  progress 'Platform: waiting for nodes and cluster networking'
  "${KUBECTL[@]}" -n kube-system rollout status daemonset/kindnet --timeout=300s
  "${KUBECTL[@]}" wait --for=condition=Ready nodes --all --timeout=300s
  progress 'Platform: applying Gateway API resources'
  curl -fsSL https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml -o "${STAGE}/gateway-api.yaml"
  printf '24d931f22abd8e40c973264319ead7cfa09d0fb7716b7ab1ee2ff174cb063a73  %s\n' "${STAGE}/gateway-api.yaml" | shasum -a 256 -c -
  "${KUBECTL[@]}" apply --server-side -f "${STAGE}/gateway-api.yaml"
  "${KUBECTL[@]}" wait --for=condition=Established crd/gateways.gateway.networking.k8s.io crd/httproutes.gateway.networking.k8s.io --timeout=120s
  progress 'Platform: deploying Traefik and waiting for readiness'
  "${HELM}" upgrade --install platform "${HELM_DIR}/charts/platform" --kube-context "${KUBE_CONTEXT}" \
    -n gochat-system --create-namespace --reset-values --skip-crds --wait=legacy --timeout "${TIMEOUT}" \
    -f "${HELM_DIR}/values/common/platform.yaml"
  "${KUBECTL[@]}" -n gochat-system rollout status deployment/traefik --timeout=180s
  progress 'Platform ready'
}

build_images() {
  local service image image_id tag tagged_image
  render_group apps > "${STAGE}/apps.yaml"
  : > "${STAGE}/images.yaml"
  for service in api-gateway websocket-service user-service chat-service frontend; do
    image="$(awk -v prefix="go-chat-msa/${service}:" '$1 == "image:" {gsub(/"/, "", $2); if (index($2, prefix) == 1) print $2}' "${STAGE}/apps.yaml")"
    [[ -n "${image}" && "${image}" != *$'\n'* ]] || { printf 'Expected exactly one image for %s\n' "${service}" >&2; exit 1; }
    progress "${service}: building image (unchanged steps use the build cache)"
    # Omit build attestations so cached local builds retain the same image ID.
    if [[ "${service}" == frontend ]]; then
      docker build --pull --provenance=false -t "${image}" "${ROOT}/frontend"
    else
      docker build --pull --provenance=false --build-arg "SERVICE_NAME=${service}" -t "${image}" "${ROOT}"
    fi
    image_id="$(docker image inspect --format '{{.Id}}' "${image}")"
    tag="sha256-${image_id#sha256:}"
    tagged_image="${image%:*}:${tag}"
    docker tag "${image}" "${tagged_image}"
    progress "${service}: loading image into the cluster"
    kind load docker-image --name "${KIND_CLUSTER:-go-chat}" "${tagged_image}"
    printf '%s:\n  image:\n    tag: %s\n' "${service}" "${tag}" >> "${STAGE}/images.yaml"
  done
  progress 'App images ready'
}

up() {
  local job
  # Existing kubectl-managed environments must be migrated in a new namespace/cluster.
  if "${KUBECTL[@]}" -n "${NAMESPACE}" get deployment/postgres >/dev/null 2>&1 &&
     ! "${HELM}" status infra --kube-context "${KUBE_CONTEXT}" -n "${NAMESPACE}" >/dev/null 2>&1; then
    printf 'Unmanaged resources exist in %s; use a new kind cluster for Helm migration.\n' "${NAMESPACE}" >&2
    exit 1
  fi
  progress "${K8S_ENV} [1/6]: preparing charts"
  prepare_charts
  progress "${K8S_ENV} [2/6]: building and loading images"
  build_images
  "${KUBECTL[@]}" create namespace "${NAMESPACE}" --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f -
  "${KUBECTL[@]}" label namespace "${NAMESPACE}" --overwrite \
    "app.kubernetes.io/name=${NAMESPACE}" "app.kubernetes.io/instance=${NAMESPACE}" \
    app.kubernetes.io/component=namespace app.kubernetes.io/part-of=go-chat-msa
  if [[ "${K8S_ENV}" != qa ]]; then
    "${KUBECTL[@]}" label namespace "${NAMESPACE}" --overwrite \
      pod-security.kubernetes.io/audit=restricted pod-security.kubernetes.io/audit-version=v1.37
  fi
  progress "${K8S_ENV} [3/6]: deploying databases, Redis and NATS; waiting for readiness"
  upgrade_group infra
  progress "${K8S_ENV} [4/6]: deploying observability services; waiting for readiness"
  upgrade_group observability
  if [[ "${K8S_ENV}" == qa ]]; then
    "${KUBECTL[@]}" wait --for=condition=Available apiservice/v1beta1.custom.metrics.k8s.io --timeout="${TIMEOUT}"
  fi
  progress "${K8S_ENV} [5/6]: running database migrations; waiting for completion"
  "${HELM}" uninstall migrations --kube-context "${KUBE_CONTEXT}" -n "${NAMESPACE}" --ignore-not-found --wait --timeout "${TIMEOUT}"
  if ! upgrade_group migrations --wait=watcher --wait-for-jobs; then
    for job in postgres-migrate mongo-migrate; do
      "${KUBECTL[@]}" -n "${NAMESPACE}" describe "job/${job}" || true
      "${KUBECTL[@]}" -n "${NAMESPACE}" logs "job/${job}" --all-containers=true --tail=100 --pod-running-timeout=5s || true
    done
    exit 1
  fi
  progress "${K8S_ENV} [6/6]: deploying apps; waiting for apps and gateway readiness"
  upgrade_group apps -f "${STAGE}/images.yaml"
  "${KUBECTL[@]}" -n "${NAMESPACE}" wait --for=condition=Programmed gateway/gochat --timeout="${TIMEOUT}"
  progress "${K8S_ENV} ready: http://${K8S_ENV}.gochat.localhost:30080/"
}

down() {
  local release
  progress "${K8S_ENV}: removing environment (${NAMESPACE})"
  for release in load apps migrations observability infra; do
    progress "${release}: uninstalling release and waiting for cleanup"
    "${HELM}" uninstall "${release}" --kube-context "${KUBE_CONTEXT}" -n "${NAMESPACE}" --ignore-not-found --wait --timeout "${TIMEOUT}"
  done
  progress "${NAMESPACE}: waiting for namespace cleanup (may continue after the deleted message)"
  "${KUBECTL[@]}" delete namespace "${NAMESPACE}" --ignore-not-found=true --wait=true
  progress "${K8S_ENV}: environment removal complete"
}

"$1"
