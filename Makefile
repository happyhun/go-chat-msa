SHELL := /usr/bin/env bash -e -o pipefail
.NOTPARALLEL:

KIND_CLUSTER ?= go-chat
KIND_CONFIG ?= deploy/k8s/clusters/kind-local.yaml
export KIND_CLUSTER
export KUBE_CONTEXT = kind-$(KIND_CLUSTER)
HELM ?= helm
export HELM
KUBECTL = kubectl --context='$(KUBE_CONTEXT)'
KUBECTL_TIMEOUT ?= 300s
export KUBECTL_TIMEOUT
K6_LOAD_TIMEOUT ?= 30m
K6_FOLLOW_LOGS ?= false
K6_MAX_LOG_REQUESTS ?= 4
K8S_ENVS := dev test qa
HELM_SCRIPTS := deploy/helm/scripts

.PHONY: help
help:
	@printf '  %-36s %s\n' \
	  'make dev-up / test-up / qa-up' 'Build images and deploy an environment' \
	  'make dev-down / test-down / qa-down' 'Remove an environment and wait for cleanup' \
	  'make dev-load' 'Run the C10K load test (4 workers)' \
	  'make qa-load' 'Run the HPA scaling and reconnection test' \
	  'make helm-deps' 'Refresh chart dependencies from lock files' \
	  'make k8s-validate' 'Lint and render charts for every environment' \
	  'make kind-up' 'Prepare the cluster and shared platform' \
	  'make kind-tune' 'Apply node network settings' \
	  'make kind-delete' 'Delete the local cluster and its data'

.PHONY: check-prereqs check-helm helm-deps k8s-validate
check-helm:
	@source $(HELM_SCRIPTS)/common.sh; check_helm
check-prereqs: check-helm
	@for tool in docker kind kubectl go curl shasum; do \
		command -v "$$tool" >/dev/null || { printf 'Required tool not found: %s. Install it and try again.\n' "$$tool" >&2; exit 1; }; \
	done
helm-deps: check-helm
	@printf '[deploy] Refreshing chart dependencies from lock files\n'
	@source $(HELM_SCRIPTS)/common.sh; prepare_dependencies --force
	@printf '[deploy] Chart dependencies ready\n'
k8s-validate: check-helm
	@bash $(HELM_SCRIPTS)/validate.sh

.PHONY: kind-up kind-tune
kind-up: check-prereqs
	@clusters=$$(kind get clusters); \
	if grep -qx '$(KIND_CLUSTER)' <<< "$$clusters"; then \
		printf '[deploy] Using existing cluster: $(KIND_CLUSTER)\n'; \
	else \
		printf '[deploy] Creating cluster: $(KIND_CLUSTER) (first setup may take a few minutes)\n'; \
		kind create cluster --name '$(KIND_CLUSTER)' --config '$(KIND_CONFIG)'; \
	fi
	@bash $(HELM_SCRIPTS)/deploy.sh platform
	@$(MAKE) kind-tune
	@printf '[deploy] Cluster ready: $(KIND_CLUSTER)\n'
kind-tune: check-prereqs
	@printf '[deploy] Applying node network settings and waiting for readiness\n'
	@nodes=$$(kind get nodes --name '$(KIND_CLUSTER)'); \
	for node in $$nodes; do \
		docker exec "$$node" sysctl -w net.core.somaxconn=65535 >/dev/null; \
		docker exec "$$node" sysctl -w net.ipv4.ip_local_port_range='10240 65535' >/dev/null; \
	done
	@$(KUBECTL) wait --for=condition=Ready nodes --all --timeout='$(KUBECTL_TIMEOUT)'
	@printf '[deploy] Node network settings applied; nodes ready\n'

.PHONY: $(addsuffix -up,$(K8S_ENVS)) $(addsuffix -down,$(K8S_ENVS))
$(addsuffix -up,$(K8S_ENVS)): %-up: kind-up
	@K8S_ENV=$* bash $(HELM_SCRIPTS)/deploy.sh up
$(addsuffix -down,$(K8S_ENVS)): %-down: check-helm
	@K8S_ENV=$* bash $(HELM_SCRIPTS)/deploy.sh down

.PHONY: dev-load qa-load
dev-load: check-helm
	@K8S_ENV=dev K6_LOAD_TIMEOUT='$(K6_LOAD_TIMEOUT)' K6_FOLLOW_LOGS='$(K6_FOLLOW_LOGS)' \
	K6_MAX_LOG_REQUESTS='$(K6_MAX_LOG_REQUESTS)' bash $(HELM_SCRIPTS)/load.sh
qa-load: check-helm
	@K8S_ENV=qa K6_LOAD_TIMEOUT=15m K6_FOLLOW_LOGS='$(K6_FOLLOW_LOGS)' \
	K6_MAX_LOG_REQUESTS=1 bash $(HELM_SCRIPTS)/load.sh

.PHONY: kind-delete
kind-delete:
	@printf '[deploy] Deleting cluster and its data: $(KIND_CLUSTER); waiting for completion\n'
	@kind delete cluster --name '$(KIND_CLUSTER)'
	@printf '[deploy] Cluster deletion complete: $(KIND_CLUSTER)\n'
