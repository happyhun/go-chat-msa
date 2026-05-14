# Kubernetes Dev/Test Baseline

이 디렉터리는 Phase 2 Kubernetes baseline을 위한 manifest와 bootstrap 절차를 담는다. 목표는 `kind` 클러스터에서 data layer, migration, observability, app rollout 순서를 명시하고, Ingress/probe/관측성 smoke와 fixed replicas e2e를 확인하는 것이다.

이 README는 임시 dev/test smoke runbook이다. bootstrap script, 루트 README, 운영 runbook으로 실행 절차가 흡수되어 중복 문서가 되면 제거한다.

## Scope

- `deploy/k8s/base`: 공통 manifest
- `deploy/k8s/overlays/dev`: 로컬 개발 smoke overlay
- `deploy/k8s/overlays/test`: K8s e2e correctness overlay
- `deploy/k8s/overlays/{dev,test}/{foundation,observability,migrations,apps}`: bootstrap phase overlay
- local/test data layer: Postgres, MongoDB, Redis
- migration Jobs: postgres-migrate, mongo-migrate
- observability stack: Alloy, Prometheus, Grafana, Loki, Tempo, Pyroscope
- app Deployments: api-gateway, ws-gateway, websocket-service, user-service, chat-service, frontend
- suspended CronJob: retention-job

`dev`는 단일 인스턴스 smoke, `test`는 app replicas 2 fixed e2e correctness 검증용이다. 이번 baseline에는 k6, HPA, rollout/drain 검증을 포함하지 않는다.

## Static Checks

```bash
bash -n deploy/k8s/scripts/bootstrap.sh
kubectl kustomize deploy/k8s/base
kubectl kustomize deploy/k8s/overlays/dev
kubectl kustomize deploy/k8s/overlays/dev/foundation
kubectl kustomize deploy/k8s/overlays/dev/observability
kubectl kustomize deploy/k8s/overlays/dev/migrations
kubectl kustomize deploy/k8s/overlays/dev/apps
kubectl kustomize deploy/k8s/overlays/test
kubectl kustomize deploy/k8s/overlays/test/foundation
kubectl kustomize deploy/k8s/overlays/test/observability
kubectl kustomize deploy/k8s/overlays/test/migrations
kubectl kustomize deploy/k8s/overlays/test/apps
kubectl apply --dry-run=client -k deploy/k8s/overlays/dev
kubectl apply --dry-run=client -k deploy/k8s/overlays/test
```

`kubectl apply --dry-run=client`는 OpenAPI discovery를 위해 실행 가능한 cluster context가 필요하다. 클러스터가 없으면 `kubectl kustomize`까지만 로컬 정적 확인으로 본다.

## Dev Bootstrap

```bash
kind create cluster --name go-chat --config deploy/k8s/clusters/kind-dev.yaml

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
for service in api-gateway ws-gateway websocket-service user-service chat-service retention-job; do
  docker build --build-arg SERVICE_NAME="${service}" -t "go-chat-msa/${service}:dev" .
  kind load docker-image --name go-chat "go-chat-msa/${service}:dev"
done

docker build -t go-chat-msa/frontend:dev ./frontend
kind load docker-image --name go-chat go-chat-msa/frontend:dev
```

`deploy/k8s/overlays/dev` aggregate overlay는 정적 렌더링 확인용이다. 실제 dev bootstrap은 migration과 observability 순서가 중요하므로 script를 사용한다.

```bash
K8S_ENV=dev NAMESPACE=go-chat-dev bash deploy/k8s/scripts/bootstrap.sh
```

Ingress는 `clusters/kind-dev.yaml`에서 host port `30080`으로 열어 둔다.

```bash
curl -i http://localhost:30080/
curl -i http://localhost:30080/api/health
curl -i http://localhost:30080/ws-api/health
curl -i http://localhost:30080/api/ready
curl -i http://localhost:30080/ws-api/ready
```

`/ws`는 이 브랜치에서 full WebSocket scenario까지 검증하지 않는다. 티켓 발급과 실제 upgrade 흐름은 후속 smoke/e2e 단계에서 검증한다.

Grafana는 Ingress에 연결하지 않는다. 로컬에서는 port-forward로 접근한다.

```bash
kubectl -n go-chat-dev port-forward svc/grafana 3000:3000
```

Prometheus metric smoke:

```bash
kubectl -n go-chat-dev port-forward svc/prometheus 9090:9090
curl -G 'http://localhost:9090/api/v1/query' --data-urlencode 'query=gochat_build_info'
curl -G 'http://localhost:9090/api/v1/query' --data-urlencode 'query=gochat_http_requests_total'
curl -G 'http://localhost:9090/api/v1/query' --data-urlencode 'query=gochat_grpc_requests_total'
curl -G 'http://localhost:9090/api/v1/query' --data-urlencode 'query=sum(gochat_ws_connections_active)'
```

## Test Bootstrap

Test overlay는 같은 cluster에서 별도 namespace(`go-chat-test`)로 실행한다. test image tag를 build/load한 뒤 bootstrap한다.

```bash
for service in api-gateway ws-gateway websocket-service user-service chat-service retention-job; do
  docker build --build-arg SERVICE_NAME="${service}" -t "go-chat-msa/${service}:test" .
  kind load docker-image --name go-chat "go-chat-msa/${service}:test"
done

docker build -t go-chat-msa/frontend:test ./frontend
kind load docker-image --name go-chat go-chat-msa/frontend:test

K8S_ENV=test NAMESPACE=go-chat-test bash deploy/k8s/scripts/bootstrap.sh
E2E_ENV=k8s go test -count=1 -tags e2e ./test/e2e
```

## Notes

- app image에는 `configs/base.yaml`만 포함한다. dev overlay는 `APP_ENV=k8s-dev`와 `/app/configs/k8s-dev.yaml`, test overlay는 `APP_ENV=k8s-test`와 `/app/configs/k8s-test.yaml` ConfigMap mount로 필요한 값을 K8s Secret/ConfigMap env로 override한다.
- bootstrap은 app rollout 전에 observability stack을 먼저 올린다. app telemetry endpoint는 `alloy:4318`, Pyroscope endpoint는 `http://pyroscope:4040`이다.
- migration ConfigMap은 script가 `db/migrations/postgres`, `db/migrations/mongo`에서 생성한다. migration Job 실패 시 app rollout을 진행하지 않는다.
- `retention-job`은 아직 `suspend: true`다. refresh token은 Redis TTL 기반 상태로 이관되어 별도 purge CronJob이 필요하지 않다.
- dev/test data layer와 observability storage는 개발/검증용이며 운영 승격 대상이 아니다.
- dev/test overlay는 MongoDB 8.0과 Linux kernel 6.19 조합의 startup crash를 피하기 위해 `GLIBC_TUNABLES=glibc.pthread.rseq=1`를 주입한다.
