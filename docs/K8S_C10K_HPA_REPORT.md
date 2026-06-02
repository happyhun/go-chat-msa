# Kubernetes C10K/HPA 검증 리포트

> 이 문서는 로컬 kind K8s 실행 경로에서 수행하는 `dev-load`와 `qa-load`를 설명합니다.
> Docker Compose 기준 C10K baseline은 [DOCKER_C10K_REPORT.md](DOCKER_C10K_REPORT.md), 그때의 병목 해결 기록은 [DOCKER_C10K_TROUBLESHOOTING.md](DOCKER_C10K_TROUBLESHOOTING.md)를 봅니다.

---

## 1. 목적

K8s 이관 후 부하테스트는 두 가지로 분리합니다.

| 명령 | 스크립트 | 환경 | 목적 |
|------|----------|------|------|
| `make dev-load` | `test/load/c10k-test.js` | `go-chat-dev` | C10K 부하 경로가 K8s에서도 동작하는지 확인 |
| `make qa-load` | `test/load/hpa-test.js` | `go-chat-qa` | WebSocket HPA `1→2` handoff 중 메시지 정합성 검증 |

`dev-load`와 `qa-load`는 같은 k6라도 합격 기준이 다릅니다.

- `dev-load`: 연결 수, 메시지 처리량, 지연 시간, timeout을 보는 부하/성능 경로
- `qa-load`: HPA scale-out 중 sequence 중복/역전, sync gap 회복, 정상 handoff의 DB sequence hole을 보는 정합성 경로

---

## 2. 실행 구조

두 테스트 모두 k6를 로컬 호스트가 아니라 Kubernetes Job으로 실행합니다.

```text
k6 Job Pod
  → api-gateway:8080
  → ws-gateway:8088
```

테스트 클라이언트가 클러스터 안에서 `ClusterIP` Service로 접근하므로, 로컬 브라우저 경로의 ingress-nginx를 타지 않습니다.
이 선택은 k6와 서버를 같은 K8s 네트워크 모델 안에 두고, Docker Compose 시절처럼 컨테이너 간 통신 기준으로 검증하기 위한 것입니다.

kind 클러스터는 control-plane 1개와 worker 1개로 구성합니다.
control-plane은 ingress-nginx와 host port 진입점을 담당하고, 앱 Pod는 worker에 스케줄됩니다.

---

## 3. dev-load: K8s C10K 경로

### 실행

```bash
make dev-up
make dev-load
```

`dev-load`는 `k6-c10k` Indexed Job을 실행합니다.
Job은 k6 Pod 4개로 나뉘고, 각 Pod는 `K6_VU_OFFSET`과 `K6_TARGET_VUS`로 VU 범위를 나눠 가집니다.

### 부하 프로필

| 항목 | 값 |
|------|-----|
| 총 VU | 10,000 |
| k6 Pod | 4 |
| worker당 VU | 2,500 |
| ramp-up | 10분 |
| steady | 3분 |
| ramp-down | 2분 |
| 채팅방 | 100개 |
| 방당 인원 | 100명 |
| 메시지 주기 | 5초 |
| 예상 ingress | 2K messages/sec |
| 예상 egress | 200K messages/sec |

### k6 threshold

| 메트릭 | 기준 |
|--------|------|
| `msg_latency` | P99 < 50ms |
| `history_fetch_duration` | P99 < 100ms |
| `sync_fetch_duration` | P99 < 100ms |
| `ws_connect_errors` | count < 1 |
| `msg_timeouts` | count < 1 |

### K8s 설정 기준

`dev`는 Docker Compose 시절 C10K 조건과 최대한 맞춘 개발 기준선입니다.

- `websocket-service`: 2 replicas
- `user-service`: CPU limit 4
- 주요 앱 Pod: `net.core.somaxconn=65535`
- kind node: `net.core.somaxconn=65535`, `net.ipv4.ip_local_port_range=10240 65535`
- k6 Job: `ulimit -n` 출력, `ip_local_port_range` 출력
- 앱 메모리 limit: dev에서는 두지 않음

### 해석 기준

K8s `dev-load`는 Docker Compose baseline과 같은 부하 시나리오를 K8s 실행 경로에서 재현하는 테스트입니다.
하지만 로컬 kind는 Docker 컨테이너 안의 Kubernetes node, kube-proxy, CNI, observability stack까지 함께 사용하므로 Docker Compose 성능 수치와 1:1로 같아야 한다고 해석하면 안 됩니다.

포트폴리오에서 성능 기준 수치로 제시할 값은 Docker Compose C10K baseline을 사용하고, K8s `dev-load`는 “K8s 이관 후 동일 부하 경로가 동작하며 병목을 관측할 수 있다”는 검증으로 설명합니다.

---

## 4. qa-load: WebSocket HPA 정합성

### 실행

```bash
make qa-up
make qa-load
```

`qa-load`는 `k6-hpa` Job을 실행합니다.
시작 전에 `deploy/k8s/scripts/load.sh`가 HPA 시작 상태를 reset합니다.

```text
기존 k6-hpa Job 삭제
HPA 삭제
websocket-service replicas=1
rollout 대기
HPA 재적용
k6-hpa Job 실행
```

이 reset이 필요한 이유는 HPA scale-down stabilization 때문입니다.
이전 테스트 직후 `websocket-service`가 2 replicas로 남아 있으면, 다음 테스트에서 `1→2` handoff를 재현하지 못합니다.

### HPA 설정

| 항목 | 값 |
|------|-----|
| 대상 | `deployment/websocket-service` |
| min replicas | 1 |
| max replicas | 2 |
| metric type | Pods custom metric |
| metric name | `gochat_ws_connections_active` |
| target | `AverageValue: 100` |
| scale up | 1 Pod / 30s |
| scale down stabilization | 300s |

metric 경로:

```text
websocket-service
  → gochat_ws_connections_active
  → Alloy
  → Prometheus
  → Prometheus Adapter
  → custom.metrics.k8s.io
  → HPA
```

### 부하 프로필

| 항목 | 값 |
|------|-----|
| 총 VU | 300 |
| 채팅방 | 30개 |
| 방당 인원 기준 | 100명 |
| 메시지 주기 | 5초 |
| WebSocket 유지 | 90초 |
| connect 503 retry | 최대 3회, 250~1500ms jitter |
| sync gap retry | 최대 3회, 500~3500ms jitter |

이 부하는 C10K가 아닙니다.
로컬 kind에서 HPA scale-out을 확실히 만들면서, handoff 중 메시지 정합성을 확인하기 위한 작은 시나리오입니다.

### k6 threshold

| 메트릭 | 기준 |
|--------|------|
| `auth_errors` | count < 1 |
| `join_errors` | count < 1 |
| `ticket_errors` | count < 1 |
| `ws_connect_errors` | count < 1 |
| `msg_timeouts` | count < 1 |
| `ws_sequence_duplicates` | count < 1 |
| `ws_sequence_regressions` | count < 1 |
| `sync_sequence_duplicates` | count < 1 |
| `sync_sequence_regressions` | count < 1 |
| `sync_gap_discarded` | count < 1 |

### 최종 확인 결과

최근 QA 실행에서는 HPA scale-out과 정합성 기준을 통과했습니다.

| 항목 | 결과 |
|------|------|
| HPA 동작 | `websocket-service` 1 → 2 |
| k6 Job | `k6-hpa Complete 1/1` |
| HTTP failure | 0 |
| WebSocket error | 0 |
| sequence duplicate | 0 |
| sequence regression | 0 |
| sync gap observed/recovered | 2 / 2 |
| sync gap discarded | 0 |
| Mongo sequence hole | 0 |
| message latency P99 | 4ms |
| sync fetch P99 | 15.33ms |

이 결과의 의미는 “HPA 중 메시지 정합성이 깨지지 않았다”입니다.
운영 성능 수치나 C10K 성능 수치로 해석하지 않습니다.

---

## 5. 결과 해석 원칙

| 질문 | 기준 문서 |
|------|----------|
| 포트폴리오 성능 baseline은 무엇인가? | [DOCKER_C10K_REPORT.md](DOCKER_C10K_REPORT.md) |
| C10K 병목을 어떻게 해결했는가? | [DOCKER_C10K_TROUBLESHOOTING.md](DOCKER_C10K_TROUBLESHOOTING.md) |
| K8s에서도 C10K 경로를 어떻게 돌리는가? | 이 문서의 `dev-load` |
| WebSocket HPA 중 정합성은 어떻게 검증하는가? | 이 문서의 `qa-load` |
| HPA handoff 설계는 왜 필요한가? | [velog-websocket-hpa-handoff.md](velog-websocket-hpa-handoff.md) |

Docker Compose baseline과 K8s local kind 결과를 같은 표에서 우열 비교하지 않습니다.
전자는 순수 로컬 컨테이너 성능 기준이고, 후자는 K8s 이관 후 실행 경로와 동적 handoff 정합성 검증입니다.
