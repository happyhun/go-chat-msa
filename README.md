# Go Chat MSA

Go와 Kubernetes로 구현한 실시간 채팅 MSA입니다.

이 프로젝트의 목표는 채팅방 메시지를 빠르게 분배하면서도 메시지 순번 정합성과 수평 확장성을 함께 지키는 것입니다. 사용자/방 관리, 메시지 저장, 실시간 WebSocket 연결을 서로 다른 서비스로 나누고, 로컬 kind Kubernetes 환경에서 C10K 부하와 WebSocket Service HPA 확장 중 메시지 정합성을 검증했습니다.

## 핵심 결과

| 검증 | 환경 | 결과 |
| :--- | :--- | :--- |
| C10K 부하 테스트 | K8s `dev`, k6 Pod 4개 | 10,000 연결, 메시지 입력 약 2K/s, 메시지 분배 약 200K/s, 메시지 P99 최대 43ms, timeout 0 |
| WebSocket HPA 정합성 | K8s `qa`, WebSocket Service HPA `1→2` | 순번 중복 0, 순번 역전 0, 누락 구간 회복 2/2, MongoDB 순번 공백 0 |
| 멀티 인스턴스 E2E | K8s `test`, 주요 서비스 2 replicas | 인증, 방 생성/입장, 메시지 송수신, 재연결 복구, Redis 멤버십 검증 |

상세 수치는 [Kubernetes C10K 부하 테스트 보고서](docs/K8S_C10K_REPORT.md)와 아래 검증 결과에 정리되어 있습니다.

## 설계 포인트

- **Consistent Hashing 기반 방 라우팅**: Redis 후보 목록으로 해시 링을 만들고 `room_id` 기준 담당 인스턴스를 선택해, 같은 방 세션을 한 WebSocket Service 인스턴스로 모읍니다.
- **방 소유권과 순번 정합성**: 방 소유권, 마지막 순번 기준값, 종료 시 저장 완료 대기를 통해 담당 인스턴스 교체 중 이미 발급된 메시지 순번이 다시 쓰이지 않게 했습니다.
- **실시간 전송과 저장 분리**: 메시지 브로드캐스트 경로와 MongoDB 배치 저장 경로를 분리해 전송 지연을 낮추고, 저장 큐가 가득 차면 메시지를 먼저 거절합니다.
- **단방향 계층 구조**: Router, Manager, Hub, Session의 책임을 나누고 상향 의존을 채널과 콜백으로 제한해 WebSocket 상태 변경 순서를 추적하기 쉽게 했습니다.
- **Bcrypt 워커 풀**: 회원가입과 로그인 해싱은 CPU 사용량이 큰 작업이므로 워커 풀로 동시 실행 수를 제한하고, 대기열이 가득 차면 빠르게 실패시켜 연쇄 지연을 막습니다.
- **UUID v7 ID 생성**: 주요 ID를 애플리케이션에서 생성해 INSERT 전에 식별자를 확정하고, 시간순 정렬성과 B-tree 인덱스 삽입 지역성을 높였습니다.

## 주요 의사결정

### WebSocket 방 담당 인스턴스

WS Gateway는 Redis에 등록된 WebSocket Service 후보 목록으로 해시 링을 만들고, `room_id` 기준 consistent hashing으로 담당 인스턴스를 선택합니다. 같은 채팅방의 세션을 한 인스턴스로 모은 뒤, 메시지 분배는 해당 인스턴스의 메모리에서 처리합니다. Redis Pub/Sub은 메시지 처리 경로에 넣지 않고, Redis는 후보 목록, 방 소유권, 마지막 순번 기준값처럼 제어 상태만 관리합니다.

### 방 소유권과 순번 기준값

HPA나 rollout으로 방 담당 인스턴스가 바뀌면 기존 인스턴스는 새 메시지 수신을 막고, 이미 브로드캐스트한 메시지의 저장 완료를 기다립니다. 저장 실패가 확정되면 마지막 발급 순번을 Redis에 기록합니다. 새 담당 인스턴스는 MongoDB 마지막 순번과 Redis의 마지막 순번 기준값 중 큰 값부터 시작해 이미 사용한 순번을 다시 쓰지 않습니다.

### 실시간 전송과 저장 분리

메시지는 저장 큐 슬롯을 먼저 확보한 뒤 순번을 부여하고 브로드캐스트합니다. 실제 MongoDB 저장은 배치 워커가 비동기로 처리합니다. 저장 큐가 가득 차면 메시지를 먼저 거절하므로, 사용자에게 전달됐지만 저장되지 않는 상태를 만들지 않습니다.

### WebSocket 계층 구조

WebSocket Service는 Router, Manager, Hub, Session으로 나눴습니다. Router는 연결 요청을 검증하고, Manager는 방 소유권과 Hub 생명주기를 관리하며, Hub는 한 채팅방의 세션과 브로드캐스트를 담당합니다. 자식 계층이 부모 구현체를 직접 참조하지 않도록 채널, 콜백, 인터페이스로 의존 방향을 제한했습니다.

### Bcrypt 워커 풀

회원가입과 로그인은 bcrypt 해싱 때문에 CPU 사용량이 큽니다. 요청마다 제한 없이 고루틴을 만들면 해싱 처리량보다 스케줄링 경합이 먼저 커질 수 있으므로, `runtime.GOMAXPROCS(0)` 기준 워커 풀로 동시에 실행되는 해싱 수를 제한합니다. 대기열이 가득 차면 즉시 실패시켜 인증 요청 지연이 서비스 전체로 번지지 않게 했습니다.

## 아키텍처

| 서비스 | 책임 | 통신 | 상태/저장소 |
| :--- | :--- | :--- | :--- |
| `api-gateway` | REST API 진입점, JWT 검증 | HTTP | Redis (요청 제한) |
| `ws-gateway` | WebSocket 티켓 발급, 방 기준 인스턴스 선택, WebSocket 프록시 | HTTP/WebSocket | Redis (티켓, 요청 제한, WebSocket Service 인스턴스 목록) |
| `websocket-service` | 세션 관리, 방 단위 브로드캐스트, 메시지 순번과 방 소유권 관리 | WebSocket | Redis (방 소유권, 마지막 순번 기준) |
| `user-service` | 사용자, 채팅방, 멤버십, refresh token 관리 | gRPC | PostgreSQL, Redis (refresh token 상태) |
| `chat-service` | 메시지 저장, 이력 조회, 누락 메시지 조회 | gRPC | MongoDB |
| `frontend` | 데모 UI | HTTP | - |

주요 구조는 다이어그램으로 분리했습니다.

| 다이어그램 | 내용 |
| :--- | :--- |
| [MSA 앱 아키텍처](docs/diagrams/flow-msa.mmd) | 서비스 책임과 데이터 저장소 경계 |
| [K8s 런타임 배포 구조](docs/diagrams/flow-k8s-runtime.mmd) | Ingress, 서비스, 데이터 계층, 관측성 구성 |
| [WebSocket 라우팅 비교](docs/diagrams/flow-ws-routing.mmd) | 정적 라우팅과 Redis 후보 목록 기반 동적 라우팅 비교 |
| [메시지 처리 흐름](docs/diagrams/flow-message.mmd) | WebSocket 메시지 수신, 순번 부여, 브로드캐스트, 저장 |
| [로그인과 WebSocket 인증](docs/diagrams/seq-auth-ticket.mmd) | access token, refresh token, WebSocket ticket 흐름 |
| [스케일아웃 시 재배치](docs/diagrams/seq-rebalance.mmd) | HPA 확장 중 담당 인스턴스 이전과 종료 절차 |

상세 설계와 의사결정은 [시스템 설계 문서](docs/DESIGN.md)에 정리되어 있습니다.

## 기술 스택

| 영역 | 사용 기술 |
| :--- | :--- |
| 언어/런타임 | Go 1.26 |
| 외부 통신 | HTTP, WebSocket |
| 내부 통신 | gRPC, Protobuf, Buf |
| 저장소 | PostgreSQL 17, MongoDB 7.0, Redis 7 |
| 인증 | JWT access token, HttpOnly refresh token, Redis TTL 기반 rotation |
| 라우팅 | Consistent Hashing, Redis membership, 방 소유권 관리 |
| 배포 | Kubernetes, Kustomize, kind, ingress-nginx, Prometheus Adapter |
| 테스트 | Go test, Testcontainers, K8s E2E, k6 |
| 관측성 | OpenTelemetry, Grafana, Prometheus, Loki, Tempo, Pyroscope |

## 화면

> 프론트엔드는 데모 목적이며, 핵심은 백엔드 분산 설계와 Kubernetes 검증입니다.

| 로비 | 채팅 |
| :---: | :---: |
| ![로비](docs/images/스크린샷_로비.png) | ![채팅](docs/images/스크린샷_채팅방.png) |

| 대시보드 |
| :---: |
| ![대시보드](docs/images/스크린샷_대시보드.png) |

## Kubernetes 실행 환경

로컬 Kubernetes는 kind 기준입니다. 실행 목적에 따라 `dev`, `test`, `qa` overlay를 분리했습니다.

| 환경 | 명령 | 목적 | 주요 설정 |
| :--- | :--- | :--- | :--- |
| `dev` | `make dev-up` | 개발 확인, K8s C10K 부하 테스트 | `websocket-service` 2 replicas |
| `test` | `make test-up` | 자동화된 E2E 시나리오 검증 | 주요 gateway/service 2 replicas |
| `qa` | `make qa-up` | WebSocket HPA 확장 중 메시지 정합성 검증 | `websocket-service` HPA `1→2` |

kind 클러스터는 control-plane 1개와 worker 1개로 구성합니다. Ingress controller는 control-plane의 host port `30080`으로 노출하고, 애플리케이션 Pod는 worker에 스케줄합니다.

### 준비 도구

| 도구 | 용도 |
| :--- | :--- |
| Docker | 이미지 빌드, kind node runtime |
| kind | 로컬 Kubernetes 클러스터 |
| kubectl | K8s 리소스 적용/조회 |
| Go | 테스트 실행 |
| Make | 표준 명령 실행 |

권장 로컬 사양은 CPU 4코어, 메모리 16GB 이상입니다. `dev`, `test`, `qa`를 동시에 띄우거나 k6 부하를 반복 실행하면 더 많은 메모리가 필요합니다.

### 실행

```bash
make dev-up
```

`make dev-up`은 kind 클러스터 생성, ingress-nginx 설치, 커널 튜닝, 이미지 빌드/load, K8s bootstrap을 순서대로 수행합니다.

| 서비스 | URL |
| :--- | :--- |
| Frontend | http://dev.gochat.localhost:30080/ |
| API health | http://dev.gochat.localhost:30080/api/health |
| WS health | http://dev.gochat.localhost:30080/ws-api/health |
| OpenAPI | http://dev.gochat.localhost:30080/docs/ |
| Grafana | http://dev.gochat.localhost:30080/grafana/ |

다른 환경은 host만 바뀝니다.

| 환경 | Host |
| :--- | :--- |
| `dev` | `dev.gochat.localhost:30080` |
| `test` | `test.gochat.localhost:30080` |
| `qa` | `qa.gochat.localhost:30080` |

### 검증

K8s manifest 렌더링을 먼저 확인합니다.

```bash
make k8s-validate
```

E2E는 `test` overlay가 올라간 상태에서 실행합니다.

```bash
make test-up
go test -count=1 -tags=e2e ./test/e2e
```

통합 테스트까지 함께 확인할 때는 build tag를 같이 넘깁니다.

```bash
go test -count=1 -tags=integration,e2e ./...
```

### 부하 테스트

K8s C10K 부하 테스트는 `dev` 환경에서 실행합니다.

```bash
make dev-load
```

`dev-load`는 K8s 내부에서 k6 Pod 4개를 띄워 `test/load/c10k-test.js`를 실행합니다. 단일 k6 프로세스의 JS 실행 병목을 피하기 위해 10,000 VU를 4개 워커로 나눕니다.

HPA 정합성 테스트는 `qa` 환경에서 실행합니다.

```bash
make qa-load
```

`qa-load`는 시작 전 `websocket-service`를 1 replica로 되돌린 뒤 HPA를 다시 붙입니다. 목적은 WebSocket Service가 `1→2`로 확장되는 동안 방 담당 인스턴스 이전이 메시지 순번 정합성을 깨지 않는지 확인하는 것입니다.

### 정리

```bash
make dev-down
make test-down
make qa-down
make kind-delete
```

## 검증 결과

### K8s C10K

로컬 kind `dev` 환경에서 K8s 기준 C10K 부하 경로를 재검증했습니다.

| 항목 | 결과 |
| :--- | :--- |
| 동시 접속 | 10,000 |
| 채팅방 | 100개 |
| 방당 인원 | 100명 |
| 메시지 입력 | 약 2K msg/s |
| 메시지 분배 | 약 200K msg/s |
| 클라이언트 메시지 P99 | 최대 43ms |
| 서버 Fanout P99 | 6.51ms |
| 서버 Egress P99 | 24.2ms |
| 메시지 timeout | 0 |

자세한 테스트 환경, 워커별 수치, CPU/메모리 분석은 [Kubernetes C10K 부하 테스트 보고서](docs/K8S_C10K_REPORT.md)를 참고하세요.

### WebSocket HPA 정합성

로컬 kind `qa` 환경에서 `websocket-service` HPA `1→2` 확장 중 메시지 정합성 시나리오를 통과했습니다.

| 항목 | 결과 |
| :--- | :--- |
| k6 작업 | `k6-hpa Complete 1/1` |
| HTTP 실패 | 0 |
| WebSocket 오류 | 0 |
| 순번 중복 | 0 |
| 순번 역전 | 0 |
| 누락 구간 감지/복구 | 2 / 2 |
| 누락 구간 미복구 | 0 |
| MongoDB 순번 공백 | 0 |
| 메시지 지연 P99 | 4ms |
| 누락분 조회 P99 | 15.33ms |

이 수치는 운영 성능 지표가 아니라 HPA 확장 중 방 담당 인스턴스 이전이 정합성을 깨지 않는지 확인한 결과입니다.

### Docker Compose 기준점

Docker Compose는 더 이상 기본 실행 경로가 아닙니다. 다만 K8s 전환 전 C10K 기준점은 `legacy-compose-baseline` 태그와 문서로 남겼습니다.

| 문서 | 내용 |
| :--- | :--- |
| [DOCKER_C10K_REPORT.md](docs/DOCKER_C10K_REPORT.md) | Docker Compose C10K 성능 기준 |
| [DOCKER_C10K_TROUBLESHOOTING.md](docs/DOCKER_C10K_TROUBLESHOOTING.md) | Docker Compose C10K 병목과 해결 기록 |

## 문서

| 문서 | 내용 |
| :--- | :--- |
| [DESIGN.md](docs/DESIGN.md) | 전체 설계와 트레이드오프 |
| [K8S_C10K_REPORT.md](docs/K8S_C10K_REPORT.md) | K8s `dev-load` C10K 부하 테스트 결과 |
| [TELEMETRY_CATALOG.md](docs/TELEMETRY_CATALOG.md) | 로그/메트릭/트레이스/프로파일 카탈로그 |
| [DOCKER_C10K_REPORT.md](docs/DOCKER_C10K_REPORT.md) | Docker Compose C10K 성능 기준 |
| [DOCKER_C10K_TROUBLESHOOTING.md](docs/DOCKER_C10K_TROUBLESHOOTING.md) | Docker Compose C10K 병목과 해결 기록 |

## 운영 경계

이 프로젝트의 K8s manifest는 로컬 검증용입니다. PostgreSQL, MongoDB, Redis, 관측성 스택은 kind 안에 함께 띄웁니다.

운영으로 확장하려면 managed DB, 백업/복구, Redis HA, Secret 관리, RBAC, NetworkPolicy, 이미지 태그 정책, 멀티 노드 rollout/drain runbook을 별도로 설계해야 합니다.
