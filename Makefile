SHELL := /usr/bin/env bash
.NOTPARALLEL:

KIND_CLUSTER ?= go-chat
KIND_CONFIG ?= deploy/k8s/clusters/kind-local.yaml
KUBECTL_TIMEOUT ?= 180s
K6_JOB_NAME ?= k6-c10k
K6_LOAD_TIMEOUT ?= 30m
K6_FOLLOW_LOGS ?= true
K6_MAX_LOG_REQUESTS ?= 4

GO_SERVICES := api-gateway ws-gateway websocket-service user-service chat-service
K8S_KUSTOMIZE_TARGETS := \
	deploy/k8s/base \
	deploy/k8s/base/foundation \
	deploy/k8s/base/observability \
	deploy/k8s/base/migrations \
	deploy/k8s/base/apps \
	deploy/k8s/base/load \
	deploy/k8s/overlays/dev \
	deploy/k8s/overlays/dev/foundation \
	deploy/k8s/overlays/dev/observability \
	deploy/k8s/overlays/dev/migrations \
	deploy/k8s/overlays/dev/apps \
	deploy/k8s/overlays/dev/load \
	deploy/k8s/overlays/test \
	deploy/k8s/overlays/test/foundation \
	deploy/k8s/overlays/test/observability \
	deploy/k8s/overlays/test/migrations \
	deploy/k8s/overlays/test/apps \
	deploy/k8s/overlays/test/load \
	deploy/k8s/overlays/qa \
	deploy/k8s/overlays/qa/foundation \
	deploy/k8s/overlays/qa/observability \
	deploy/k8s/overlays/qa/migrations \
	deploy/k8s/overlays/qa/apps \
	deploy/k8s/overlays/qa/load

.PHONY: help
help:
	@printf 'Targets:\n'
	@printf '  make dev-up          Create kind cluster, build/load dev images, bootstrap dev overlay\n'
	@printf '  make test-up         Create kind cluster, build/load test images, bootstrap test overlay\n'
	@printf '  make qa-up           Create kind cluster, build/load qa images, bootstrap qa overlay\n'
	@printf '  make dev-load        Run k6 C10K load test with 4 k6 worker pods\n'
	@printf '  make qa-load         Run k6 HPA consistency test in qa Kubernetes namespace\n'
	@printf '  make k8s-validate    Render all Kustomize bases/overlays\n'
	@printf '  make dev-down        Delete dev namespace\n'
	@printf '  make test-down       Delete test namespace\n'
	@printf '  make qa-down         Delete qa namespace\n'
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

.PHONY: k8s-validate
k8s-validate: check-kubectl
	@for target in $(K8S_KUSTOMIZE_TARGETS); do \
		printf 'kubectl kustomize %s\n' "$$target"; \
		kubectl kustomize "$$target" >/dev/null; \
	done

.PHONY: kind-up
kind-up: check-prereqs
	@if kind get clusters | grep -qx '$(KIND_CLUSTER)'; then \
		printf 'kind cluster already exists: $(KIND_CLUSTER)\n'; \
		kubectl config use-context 'kind-$(KIND_CLUSTER)' >/dev/null; \
	else \
		kind create cluster --name '$(KIND_CLUSTER)' --config '$(KIND_CONFIG)'; \
	fi
	@$(MAKE) kind-tune
	@kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.15.1/deploy/static/provider/kind/deploy.yaml
	@kubectl -n ingress-nginx patch configmap ingress-nginx-controller \
		--type=merge \
		-p '{"data":{"use-forwarded-headers":"true","compute-full-forwarded-for":"true"}}'
	@kubectl -n ingress-nginx patch deployment ingress-nginx-controller \
		--type=merge \
		-p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/os":"linux","ingress-ready":"true"},"securityContext":{"sysctls":[{"name":"net.core.somaxconn","value":"65535"},{"name":"net.ipv4.ip_local_port_range","value":"10240 65535"}]}}}}}'
	@kubectl wait --namespace ingress-nginx \
		--for=condition=ready pod \
		--selector=app.kubernetes.io/component=controller \
		--timeout='$(KUBECTL_TIMEOUT)'

.PHONY: kind-tune
kind-tune: check-prereqs
	@kubectl config use-context 'kind-$(KIND_CLUSTER)' >/dev/null
	@for node in $$(kind get nodes --name '$(KIND_CLUSTER)'); do \
		printf 'tuning kernel sysctls on %s\n' "$$node"; \
		docker exec "$$node" sysctl -w net.core.somaxconn=65535 >/dev/null; \
		docker exec "$$node" sysctl -w net.ipv4.ip_local_port_range='10240 65535' >/dev/null; \
		docker exec "$$node" bash -lc ' \
			set -eu; \
			cfg=/var/lib/kubelet/config.yaml; \
			changed=0; \
			if ! grep -q "^allowedUnsafeSysctls:" "$$cfg"; then \
				printf "\nallowedUnsafeSysctls:\n- net.core.somaxconn\n- net.ipv4.ip_local_port_range\n" >> "$$cfg"; \
				changed=1; \
			else \
				if ! grep -q "^- net.core.somaxconn" "$$cfg"; then \
					sed -i "/^allowedUnsafeSysctls:/a - net.core.somaxconn" "$$cfg"; \
					changed=1; \
				fi; \
				if ! grep -q "^- net.ipv4.ip_local_port_range" "$$cfg"; then \
					sed -i "/^allowedUnsafeSysctls:/a - net.ipv4.ip_local_port_range" "$$cfg"; \
					changed=1; \
				fi; \
			fi; \
			if [ "$$changed" = 1 ]; then systemctl restart kubelet; fi'; \
	done
	@kubectl wait --for=condition=Ready nodes --all --timeout='$(KUBECTL_TIMEOUT)'

.PHONY: build-load-dev-images
build-load-dev-images: kind-up
	@for service in $(GO_SERVICES); do \
		docker build --build-arg SERVICE_NAME="$$service" -t "go-chat-msa/$$service:dev" .; \
		kind load docker-image --name '$(KIND_CLUSTER)' "go-chat-msa/$$service:dev"; \
	done
	@docker build -t go-chat-msa/frontend:dev ./frontend
	@kind load docker-image --name '$(KIND_CLUSTER)' go-chat-msa/frontend:dev

.PHONY: build-load-test-images
build-load-test-images: kind-up
	@for service in $(GO_SERVICES); do \
		docker build --build-arg SERVICE_NAME="$$service" -t "go-chat-msa/$$service:test" .; \
		kind load docker-image --name '$(KIND_CLUSTER)' "go-chat-msa/$$service:test"; \
	done
	@docker build -t go-chat-msa/frontend:test ./frontend
	@kind load docker-image --name '$(KIND_CLUSTER)' go-chat-msa/frontend:test

.PHONY: build-load-qa-images
build-load-qa-images: kind-up
	@for service in $(GO_SERVICES); do \
		docker build --build-arg SERVICE_NAME="$$service" -t "go-chat-msa/$$service:qa" .; \
		kind load docker-image --name '$(KIND_CLUSTER)' "go-chat-msa/$$service:qa"; \
	done
	@docker build -t go-chat-msa/frontend:qa ./frontend
	@kind load docker-image --name '$(KIND_CLUSTER)' go-chat-msa/frontend:qa

.PHONY: dev-up
dev-up: kind-up build-load-dev-images
	@K8S_ENV=dev NAMESPACE=go-chat-dev KUBECTL_TIMEOUT='$(KUBECTL_TIMEOUT)' bash deploy/k8s/scripts/bootstrap.sh

.PHONY: test-up
test-up: kind-up build-load-test-images
	@K8S_ENV=test NAMESPACE=go-chat-test KUBECTL_TIMEOUT='$(KUBECTL_TIMEOUT)' bash deploy/k8s/scripts/bootstrap.sh

.PHONY: qa-up
qa-up: kind-up build-load-qa-images
	@K8S_ENV=qa NAMESPACE=go-chat-qa KUBECTL_TIMEOUT='$(KUBECTL_TIMEOUT)' bash deploy/k8s/scripts/bootstrap.sh

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

.PHONY: dev-down
dev-down:
	kubectl delete namespace go-chat-dev --ignore-not-found=true

.PHONY: test-down
test-down:
	kubectl delete namespace go-chat-test --ignore-not-found=true

.PHONY: qa-down
qa-down:
	kubectl delete namespace go-chat-qa --ignore-not-found=true

.PHONY: kind-delete
kind-delete:
	kind delete cluster --name '$(KIND_CLUSTER)'
