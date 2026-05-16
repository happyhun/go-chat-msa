SHELL := /usr/bin/env bash
.NOTPARALLEL:

KIND_CLUSTER ?= go-chat
KIND_CONFIG ?= deploy/k8s/clusters/kind-dev.yaml
KUBECTL_TIMEOUT ?= 180s

GO_SERVICES := api-gateway ws-gateway websocket-service user-service chat-service

.PHONY: help
help:
	@printf 'Targets:\n'
	@printf '  make dev-up          Create kind cluster, build/load dev images, bootstrap dev overlay\n'
	@printf '  make test-up         Create kind cluster, build/load test images, bootstrap test overlay\n'
	@printf '  make dev-down        Delete dev namespace\n'
	@printf '  make test-down       Delete test namespace\n'
	@printf '  make kind-delete     Delete local kind cluster\n'

.PHONY: check-prereqs
check-prereqs:
	@command -v docker >/dev/null || { printf 'missing required command: docker\n' >&2; exit 1; }
	@command -v kind >/dev/null || { printf 'missing required command: kind\n' >&2; exit 1; }
	@command -v kubectl >/dev/null || { printf 'missing required command: kubectl\n' >&2; exit 1; }
	@command -v go >/dev/null || { printf 'missing required command: go\n' >&2; exit 1; }

.PHONY: kind-up
kind-up: check-prereqs
	@if kind get clusters | grep -qx '$(KIND_CLUSTER)'; then \
		printf 'kind cluster already exists: $(KIND_CLUSTER)\n'; \
		kubectl config use-context 'kind-$(KIND_CLUSTER)' >/dev/null; \
	else \
		kind create cluster --name '$(KIND_CLUSTER)' --config '$(KIND_CONFIG)'; \
	fi
	@kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.15.1/deploy/static/provider/kind/deploy.yaml
	@kubectl -n ingress-nginx patch deployment ingress-nginx-controller \
		--type=merge \
		-p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/os":"linux","ingress-ready":"true"}}}}}'
	@kubectl wait --namespace ingress-nginx \
		--for=condition=ready pod \
		--selector=app.kubernetes.io/component=controller \
		--timeout='$(KUBECTL_TIMEOUT)'

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

.PHONY: dev-up
dev-up: kind-up build-load-dev-images
	@K8S_ENV=dev NAMESPACE=go-chat-dev KUBECTL_TIMEOUT='$(KUBECTL_TIMEOUT)' bash deploy/k8s/scripts/bootstrap.sh
	@printf '\nFrontend: http://localhost:30080/\n'
	@printf 'API health: http://localhost:30080/api/health\n'
	@printf 'WS health:  http://localhost:30080/ws-api/health\n'
	@printf 'OpenAPI:    http://localhost:30080/docs/\n'
	@printf 'Grafana:    http://localhost:30080/grafana/\n'

.PHONY: test-up
test-up: kind-up build-load-test-images
	@K8S_ENV=test NAMESPACE=go-chat-test KUBECTL_TIMEOUT='$(KUBECTL_TIMEOUT)' bash deploy/k8s/scripts/bootstrap.sh

.PHONY: dev-down
dev-down:
	kubectl delete namespace go-chat-dev --ignore-not-found=true

.PHONY: test-down
test-down:
	kubectl delete namespace go-chat-test --ignore-not-found=true

.PHONY: kind-delete
kind-delete:
	kind delete cluster --name '$(KIND_CLUSTER)'
