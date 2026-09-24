SHELL := /usr/bin/env bash -e -o pipefail
.NOTPARALLEL:

KIND_CLUSTER ?= go-chat
KIND_CONFIG ?= deploy/k8s/clusters/kind-local.yaml
export KUBE_CONTEXT = kind-$(KIND_CLUSTER)
export IMAGE_TAG ?= $(shell bash deploy/k8s/scripts/image-tag.sh)
KUBECTL = kubectl --context='$(KUBE_CONTEXT)'
KUBECTL_TIMEOUT ?= 300s
K6_JOB_NAME ?= k6-c10k
K6_LOAD_TIMEOUT ?= 30m
K6_FOLLOW_LOGS ?= true
K6_MAX_LOG_REQUESTS ?= 4

GO_SERVICES := api-gateway websocket-service user-service chat-service
K8S_ENVS := dev test qa
K8S_ROOTS := deploy/k8s/base $(addprefix deploy/k8s/overlays/,$(K8S_ENVS))
K8S_KUSTOMIZE_TARGETS := $(foreach root,$(K8S_ROOTS),$(root) $(addprefix $(root)/,foundation observability migrations apps load))

.PHONY: help
help:
	@printf 'Targets:\n'
	@printf '  make dev-up          Create kind cluster, build/load dev images, bootstrap dev overlay\n'
	@printf '  make test-up         Create kind cluster, build/load test images, bootstrap test overlay\n'
	@printf '  make qa-up           Create kind cluster, build/load qa images, bootstrap qa overlay\n'
	@printf '  make dev-load        Run k6 C10K load test with 4 k6 worker pods\n'
	@printf '  make qa-load         Run k6 HPA consistency test in qa Kubernetes namespace\n'
	@printf '  make k8s-validate     Render all Kustomize bases/overlays\n'
	@printf '  make dev-down        Delete dev namespace and associated RBAC\n'
	@printf '  make test-down       Delete test namespace and associated RBAC\n'
	@printf '  make qa-down         Delete qa namespace and associated RBAC\n'
	@printf '  make kind-delete     Delete local kind cluster\n'

.PHONY: check-kubectl
check-kubectl:
	@command -v kubectl >/dev/null || { printf 'missing required command: kubectl\n' >&2; exit 1; }

.PHONY: check-prereqs
check-prereqs:
	@command -v docker >/dev/null || { printf 'missing required command: docker\n' >&2; exit 1; }
	@command -v kind >/dev/null || { printf 'missing required command: kind\n' >&2; exit 1; }
	@command -v kubectl >/dev/null || { printf 'missing required command: kubectl\n' >&2; exit 1; }
	@command -v go >/dev/null || { printf 'missing required command: go\n' >&2; exit 1; }
	@command -v curl >/dev/null || { printf 'missing required command: curl\n' >&2; exit 1; }
	@command -v shasum >/dev/null || { printf 'missing required command: shasum\n' >&2; exit 1; }

.PHONY: k8s-validate
k8s-validate: check-kubectl
	@for target in $(K8S_KUSTOMIZE_TARGETS); do \
		printf 'kubectl kustomize %s\n' "$$target"; \
		kubectl kustomize "$$target" >/dev/null; \
	done

.PHONY: kind-up
kind-up: check-prereqs
	@clusters=$$(kind get clusters); \
	if grep -qx '$(KIND_CLUSTER)' <<< "$$clusters"; then \
		printf 'kind cluster already exists: $(KIND_CLUSTER)\n'; \
	else \
		kind create cluster --name '$(KIND_CLUSTER)' --config '$(KIND_CONFIG)'; \
	fi
	@bash deploy/k8s/scripts/platform.sh
	@$(MAKE) kind-tune

.PHONY: kind-tune
kind-tune: check-prereqs
	@nodes=$$(kind get nodes --name '$(KIND_CLUSTER)'); \
	for node in $$nodes; do \
		printf 'tuning kernel sysctls on %s\n' "$$node"; \
		docker exec "$$node" sysctl -w net.core.somaxconn=65535 >/dev/null; \
		docker exec "$$node" sysctl -w net.ipv4.ip_local_port_range='10240 65535' >/dev/null; \
	done
	@$(KUBECTL) wait --for=condition=Ready nodes --all --timeout='$(KUBECTL_TIMEOUT)'

.PHONY: $(addprefix build-load-,$(addsuffix -images,$(K8S_ENVS)))
$(addprefix build-load-,$(addsuffix -images,$(K8S_ENVS))): build-load-%-images: kind-up
	@for service in $(GO_SERVICES); do \
		docker build --pull --build-arg SERVICE_NAME="$$service" -t "go-chat-msa/$$service:$(IMAGE_TAG)" .; \
		kind load docker-image --name '$(KIND_CLUSTER)' "go-chat-msa/$$service:$(IMAGE_TAG)"; \
	done
	@docker build --pull -t go-chat-msa/frontend:$(IMAGE_TAG) ./frontend
	@kind load docker-image --name '$(KIND_CLUSTER)' go-chat-msa/frontend:$(IMAGE_TAG)

.PHONY: $(addsuffix -up,$(K8S_ENVS))
$(addsuffix -up,$(K8S_ENVS)): %-up: build-load-%-images
	@K8S_ENV=$* NAMESPACE=go-chat-$* KUBECTL_TIMEOUT='$(KUBECTL_TIMEOUT)' bash deploy/k8s/scripts/bootstrap.sh

.PHONY: dev-load
dev-load: check-kubectl
	@K8S_ENV=dev \
	NAMESPACE=go-chat-dev \
	K6_JOB_NAME='$(K6_JOB_NAME)' \
	K6_LOAD_TIMEOUT='$(K6_LOAD_TIMEOUT)' \
	K6_FOLLOW_LOGS='$(K6_FOLLOW_LOGS)' \
	K6_MAX_LOG_REQUESTS='$(K6_MAX_LOG_REQUESTS)' \
	bash deploy/k8s/scripts/load.sh

.PHONY: qa-load
qa-load: check-kubectl
	@K8S_ENV=qa \
	NAMESPACE=go-chat-qa \
	K6_JOB_NAME='k6-hpa' \
	K6_LOAD_TIMEOUT='15m' \
	K6_FOLLOW_LOGS='$(K6_FOLLOW_LOGS)' \
	K6_MAX_LOG_REQUESTS='1' \
	bash deploy/k8s/scripts/load.sh

.PHONY: $(addsuffix -down,$(K8S_ENVS))
$(addsuffix -down,$(K8S_ENVS)): %-down: check-kubectl
	@if [ '$*' = qa ]; then \
		$(KUBECTL) delete apiservice v1beta1.custom.metrics.k8s.io --ignore-not-found=true; \
		$(KUBECTL) delete clusterrole gochat-prometheus-adapter --ignore-not-found=true; \
		$(KUBECTL) delete clusterrolebinding gochat-prometheus-adapter gochat-prometheus-adapter-auth-delegator --ignore-not-found=true; \
		$(KUBECTL) -n kube-system delete rolebinding gochat-prometheus-adapter-auth-reader --ignore-not-found=true; \
	fi
	@$(KUBECTL) delete clusterrolebinding gochat-alloy-cadvisor-$* --ignore-not-found=true
	@$(KUBECTL) delete clusterrole gochat-alloy-cadvisor-$* --ignore-not-found=true
	@$(KUBECTL) delete namespace go-chat-$* --ignore-not-found=true

.PHONY: kind-delete
kind-delete:
	kind delete cluster --name '$(KIND_CLUSTER)'
