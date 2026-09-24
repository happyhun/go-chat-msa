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
curl -fsSL https://raw.githubusercontent.com/projectcalico/calico/v3.32.2/manifests/calico.yaml -o "${TEMP_DIR}/calico.yaml"
printf 'a8c828a06a87c629a282ebbc424895b77f3a030251993e41ea400a743675bb02  %s\n' "${TEMP_DIR}/calico.yaml" | shasum -a 256 -c -
cat > "${TEMP_DIR}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - calico.yaml
images:
  - name: quay.io/calico/cni
    newTag: v3.32.2
  - name: quay.io/calico/node
    newTag: v3.32.2
  - name: quay.io/calico/kube-controllers
    newTag: v3.32.2
patches:
  - patch: |
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: calico-config
        namespace: kube-system
      data:
        calico_backend: vxlan
  - patch: |
      apiVersion: apps/v1
      kind: DaemonSet
      metadata:
        name: calico-node
        namespace: kube-system
      spec:
        template:
          spec:
            containers:
            - name: calico-node
              env:
              - name: CALICO_IPV4POOL_CIDR
                value: 10.244.0.0/16
              - name: CALICO_IPV4POOL_IPIP
                value: Never
              - name: CALICO_IPV4POOL_VXLAN
                value: Always
              - name: IP_AUTODETECTION_METHOD
                value: interface=eth0
              livenessProbe:
                exec:
                  command:
                  - /bin/calico-node
                  - -felix-live
              readinessProbe:
                exec:
                  command:
                  - /bin/calico-node
                  - -felix-ready
YAML
"${KUBECTL[@]}" apply --server-side -k "${TEMP_DIR}"
"${KUBECTL[@]}" -n kube-system rollout status daemonset/calico-node --timeout=300s
"${KUBECTL[@]}" wait --for=condition=Ready nodes --all --timeout=300s
curl -fsSL https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml -o "${TEMP_DIR}/gateway-api.yaml"
printf '24d931f22abd8e40c973264319ead7cfa09d0fb7716b7ab1ee2ff174cb063a73  %s\n' "${TEMP_DIR}/gateway-api.yaml" | shasum -a 256 -c -
"${KUBECTL[@]}" apply --server-side -f "${TEMP_DIR}/gateway-api.yaml"
"${KUBECTL[@]}" wait --for=condition=Established crd/gateways.gateway.networking.k8s.io crd/httproutes.gateway.networking.k8s.io --timeout=120s
"${KUBECTL[@]}" apply -f "${K8S_DIR}/platform/traefik.yaml"
"${KUBECTL[@]}" -n gochat-system rollout status deployment/traefik --timeout=180s
