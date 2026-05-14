# Local Kubernetes Baseline

이 디렉터리는 Phase 2 로컬 Kubernetes baseline을 위한 첫 manifest 묶음이다. 목표는 `kind` 클러스터에서 장기 실행 서비스와 local data layer를 올리고, Ingress와 probe가 최소 smoke 수준으로 동작하는지 확인하는 것이다.

## Scope

- `deploy/k8s/base`: 공통 manifest
- `deploy/k8s/overlays/local`: local namespace와 dev image tag overlay
- local data layer: Postgres, MongoDB, Redis
- app Deployments: api-gateway, ws-gateway, websocket-service, user-service, chat-service, frontend
- suspended CronJobs: retention-job, user-token-purge-job

이번 baseline에는 migration bootstrap 자동화, observability stack, k6, HPA, rollout/drain 검증을 포함하지 않는다. DB schema가 필요한 기능 테스트는 migration bootstrap 작업 이후에 다룬다.

## Static Checks

```bash
kubectl kustomize deploy/k8s/base
kubectl kustomize deploy/k8s/overlays/local
kubectl apply --dry-run=client -k deploy/k8s/overlays/local
```

`kubectl apply --dry-run=client`는 OpenAPI discovery를 위해 실행 가능한 cluster context가 필요하다. 클러스터가 없으면 `kubectl kustomize`까지만 로컬 정적 확인으로 본다.

## Local Smoke

```bash
kind create cluster --name go-chat --config deploy/k8s/local-kind.yaml

kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.15.1/deploy/static/provider/kind/deploy.yaml
kubectl -n ingress-nginx patch deployment ingress-nginx-controller \
  --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"kubernetes.io/os":"linux","ingress-ready":"true"}}}}}'
kubectl wait --namespace ingress-nginx \
  --for=condition=ready pod \
  --selector=app.kubernetes.io/component=controller \
  --timeout=120s
```

```bash
for service in api-gateway ws-gateway websocket-service user-service chat-service retention-job user-token-purge-job; do
  docker build --build-arg SERVICE_NAME="${service}" -t "go-chat-msa/${service}:dev" .
  kind load docker-image --name go-chat "go-chat-msa/${service}:dev"
done

docker build -t go-chat-msa/frontend:dev ./frontend
kind load docker-image --name go-chat go-chat-msa/frontend:dev
```

```bash
kubectl apply -k deploy/k8s/overlays/local

kubectl -n go-chat rollout status deploy/postgres
kubectl -n go-chat rollout status deploy/mongo
kubectl -n go-chat rollout status deploy/redis
kubectl -n go-chat rollout status deploy/user-service
kubectl -n go-chat rollout status deploy/chat-service
kubectl -n go-chat rollout status deploy/api-gateway
kubectl -n go-chat rollout status deploy/websocket-service
kubectl -n go-chat rollout status deploy/ws-gateway
kubectl -n go-chat rollout status deploy/frontend
```

Ingress는 `local-kind.yaml`에서 host port `30080`으로 열어 둔다.

```bash
curl -i http://localhost:30080/
curl -i http://localhost:30080/api/health
curl -i http://localhost:30080/ws-api/health
```

`/ws`는 이 브랜치에서 full WebSocket scenario까지 검증하지 않는다. 티켓 발급과 실제 upgrade 흐름은 후속 smoke/e2e 단계에서 검증한다.

## Notes

- app image에는 `configs/base.yaml`만 포함한다. local overlay는 `APP_ENV=k8s-local`과 `/app/configs/k8s-local.yaml` ConfigMap mount로 dev observability endpoint를 끄고, 필요한 값을 K8s Secret/ConfigMap env로 override한다.
- `user-token-purge-job`과 `retention-job`은 `suspend: true`다. `user-service` 안에 token purge loop가 아직 남아 있으므로, scheduled responsibility 전환은 다음 앱 보정 작업에서 결정한다.
- local data layer manifest는 개발 검증용이며 운영 승격 대상이 아니다.
- local overlay는 MongoDB 8.0과 Linux kernel 6.19 조합의 startup crash를 피하기 위해 `GLIBC_TUNABLES=glibc.pthread.rseq=1`를 주입한다.
