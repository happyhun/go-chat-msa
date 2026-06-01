# 텔레메트리 카탈로그

## 목차

1. [개요](#1-개요)
2. [서비스별 요약](#2-서비스별-요약)
3. [헬스체크 필터링](#3-헬스체크-필터링)
4. [Logs](#4-logs)
5. [Metrics](#5-metrics)
6. [Traces](#6-traces)
7. [Profiles](#7-profiles)

---

## 1. 개요

Grafana Full Stack 기반 관측성 구성. Profiles를 제외한 시그널은 Grafana Alloy를 거쳐 각 백엔드로 라우팅.

| 시그널 | 백엔드 | 수집 방식 | 주기 | 보존 |
|--------|--------|-----------|------|------|
| Logs | Loki | K8s: Alloy가 Pod log 수집 → Loki HTTP push. Legacy compose: Docker API 로그 수집 | 1s (Alloy 기본값) | 7일 |
| Metrics | Prometheus | OTel SDK OTLP push + Alloy의 kube-state-metrics/cAdvisor scrape → Prometheus remote write | 15s | 7일 |
| Traces | Tempo | SDK가 Alloy로 OTLP HTTP push → Alloy가 Tempo로 OTLP HTTP push | 5s (SDK 기본값) | 7일 |
| Profiles | Pyroscope | SDK가 Pyroscope로 HTTP push | 15s (SDK 기본값) | 7일 |

---

## 2. 서비스별 요약

OTel resource에는 공통으로 `service.name`과 `service.instance.id`가 들어간다. `service.instance.id`는 K8s `POD_NAME`을 우선 사용하고, 로컬 단독 실행에서는 hostname으로 fallback한다.

| 서비스 | Logs | Metrics | Traces |
|--------|------|---------|--------|
| api-gateway | HTTP | HTTP, gRPC Client, Redis | HTTP, gRPC Client, Redis |
| ws-gateway | HTTP | HTTP, Routing, Redis | HTTP, Redis |
| websocket-service | HTTP | HTTP, WebSocket, Persistence, Room Lease, gRPC Client, Redis | HTTP, gRPC Client, Redis |
| user-service | gRPC | gRPC Server, PostgreSQL, Domain | gRPC Server, PostgreSQL |
| chat-service | gRPC | gRPC Server, MongoDB, Domain | gRPC Server, MongoDB |

---

## 3. 헬스체크 필터링

세 가지 시그널 모두에 적용.

### 서비스 레벨

| 대상 | 필터 |
|------|------|
| HTTP 메트릭/로그 | HTTPMetricsMiddleware, LoggingMiddleware가 `/health`, `/ready` 스킵 |
| gRPC 메트릭/로그 | UnaryServerInterceptor, UnaryLoggingInterceptor가 `grpc.health.v1.Health/*` 스킵 |
| gRPC 클라이언트 메트릭 | UnaryClientInterceptor가 `grpc.health.v1.Health/*` 스킵 |

### Alloy 레벨

| 시그널 | 필터 |
|--------|------|
| Logs | `/health`, `/ready`, `grpc.health.v1.Health/Check`, `HealthCheck`, `pg_isready`, `adminCommand: ping` 드롭 |
| Traces | `/health`, `/ready`, `grpc.health.*`, `HealthCheck` 스팬 드롭 |

---

## 4. Logs

### Common

| 필드 | 설명 |
|------|------|
| level | info, warn, error, debug |
| time | RFC3339Nano |
| msg | 로그 메시지 |
| service | 서비스명 (K8s app label 또는 앱 로그 필드) |
| source | 호출 위치 (function, file, line) |
| trace_id | OTel 트레이스 ID |
| span_id | OTel 스팬 ID |

### HTTP

| 필드 | 설명 |
|------|------|
| method | HTTP 메서드 |
| path | 요청 경로 |
| status | 응답 코드 |
| latency_ms | 처리 시간 |
| bytes_written | 응답 바이트 |
| content_length | 요청 바디 크기 |
| remote_addr | 클라이언트 주소 |
| user_agent | User-Agent 헤더 |
| query | 쿼리 파라미터 (존재 시) |
| xff | X-Forwarded-For (존재 시) |

- 5xx 응답은 ERROR 레벨
- 민감 파라미터(token, password, secret 등) 마스킹
- 서비스: api-gateway, ws-gateway, websocket-service

### gRPC

| 필드 | 설명 |
|------|------|
| method | gRPC 풀메서드 |
| code | 상태 코드 |
| latency_ms | 처리 시간 |
| error | 에러 메시지 (실패 시) |

- Internal, Unknown, DataLoss, Unavailable은 ERROR 레벨
- 서비스: user-service, chat-service

---

## 5. Metrics

### HTTP

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| gochat_http_requests_total | counter | service, method, path, status_code |
| gochat_http_request_duration_seconds | histogram | service, method, path, status_code |

- 서비스: api-gateway, ws-gateway, websocket-service

### gRPC Server

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| gochat_grpc_requests_total | counter | service, method, code |
| gochat_grpc_request_duration_seconds | histogram | service, method, code |

- 서비스: user-service, chat-service

### gRPC Client

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| gochat_grpc_client_requests_total | counter | service, method, code |
| gochat_grpc_client_request_duration_seconds | histogram | service, method, code |

- 서비스: api-gateway, websocket-service

### WebSocket

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| gochat_ws_hubs_active | gauge | - |
| gochat_ws_hubs_closed_total | counter | reason |
| gochat_ws_connections_active | gauge | - |
| gochat_ws_session_conflicts_total | counter | - |
| gochat_ws_messages_received_total | counter | - |
| gochat_ws_messages_rate_limited_total | counter | - |
| gochat_ws_messages_sent_total | counter | - |
| gochat_ws_duplicate_messages_dropped_total | counter | - |
| gochat_ws_send_queue_dropped_total | counter | - |
| gochat_ws_broadcast_channel_depth | histogram | - |
| gochat_ws_fanout_duration_seconds | histogram | - |
| gochat_ws_egress_duration_seconds | histogram | - |
| gochat_ws_rebalance_evictions_total | counter | - |
| gochat_websocket_owner_rejected_total | counter | - |
| gochat_ws_room_lease_acquire_total | counter | status |
| gochat_ws_room_lease_renew_total | counter | status |
| gochat_ws_room_handoff_total | counter | status |
| gochat_ws_room_handoff_duration_seconds | histogram | status |
| gochat_ws_sequence_conflict_total | counter | - |

- 서비스: websocket-service

### Persistence

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| gochat_ws_persist_channel_depth | gauge | - |
| gochat_ws_persist_dropped_total | counter | - |
| gochat_ws_persist_drain_total | counter | status |
| gochat_ws_persist_drain_duration_seconds | histogram | status |
| gochat_persistence_batch_save_total | counter | status |
| gochat_persistence_retry_queue_depth | gauge | - |
| gochat_persistence_retry_save_total | counter | status |
| gochat_persistence_retry_oldest_age_seconds | gauge | - |
| gochat_persistence_retry_queue_full_total | counter | - |

- 서비스: websocket-service

### PostgreSQL

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| gochat_pg_query_duration_seconds | histogram | operation |
| gochat_pg_query_total | counter | operation, status |
| gochat_pgxpool_acquired_conns | gauge | - |
| gochat_pgxpool_idle_conns | gauge | - |
| gochat_pgxpool_total_conns | gauge | - |
| gochat_pgxpool_max_conns | gauge | - |
| gochat_pgxpool_acquire_count_total | counter | - |
| gochat_pgxpool_acquire_duration_seconds_total | counter | - |
| gochat_pgxpool_empty_acquire_count_total | counter | - |
| gochat_pgxpool_canceled_acquire_count_total | counter | - |

- 서비스: user-service

### MongoDB

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| gochat_mongo_query_duration_seconds | histogram | operation |
| gochat_mongo_query_total | counter | operation, status |
| gochat_mongo_pool_checked_out_conns | gauge | - |
| gochat_mongo_pool_open_conns | gauge | - |
| gochat_mongo_pool_created_total | counter | - |
| gochat_mongo_pool_closed_total | counter | - |

- 서비스: chat-service

### Redis

`redisotel.InstrumentMetrics`로 OTel semantic convention 기반 풀 메트릭이 자동 등록됩니다. 이름은 Alloy를 거치며 점→언더스코어 변환되고, 모든 메트릭에 공통 라벨 `db_system="redis"`가 부착됩니다.

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| db_client_connections_idle_max | gauge | - |
| db_client_connections_idle_min | gauge | - |
| db_client_connections_max | gauge | - |
| db_client_connections_usage | gauge | state(idle/used) |
| db_client_connections_waits | gauge | - |
| db_client_connections_waits_duration_nanoseconds | gauge | - |
| db_client_connections_timeouts | gauge | - |
| db_client_connections_hits | gauge | - |
| db_client_connections_misses | gauge | - |
| db_client_connections_create_time_milliseconds | histogram | status, error_type |
| db_client_connections_use_time_milliseconds | histogram | type(command/pipeline), status, error_type |

명령별(GET, SET 등) latency는 메트릭이 아닌 트레이스(`redisotel.InstrumentTracing`의 span attribute)로 확인합니다. Prometheus 컨벤션은 base unit(`_seconds`)을 권장하지만 redisotel은 `_milliseconds`/`_nanoseconds`로 노출하는 라이브러리 한계가 있습니다.

- 서비스: api-gateway, ws-gateway

### Domain

| 메트릭 | 타입 | 라벨 | 서비스 |
|--------|------|------|--------|
| gochat_user_created_total | counter | status | user-service |
| gochat_user_deleted_total | counter | status | user-service |
| gochat_auth_login_total | counter | status | user-service |
| gochat_auth_token_reuse_detected_total | counter | - | user-service |
| gochat_room_join_total | counter | status | user-service |
| gochat_chat_messages_saved_total | counter | status | chat-service |
| gochat_chat_history_fetched_messages | histogram | - | chat-service |
| gochat_hasher_jobs_total | counter | type, status | user-service |
| gochat_hasher_duration_seconds | histogram | type | user-service |
| gochat_hasher_queue_depth | gauge | - | user-service |
| gochat_hasher_queue_full_total | counter | - | user-service |
| gochat_wsgateway_routed_total | counter | endpoint | ws-gateway |
| gochat_wsgateway_misdirected_total | counter | - | ws-gateway |
| gochat_membership_reconcile_total | counter | status | ws-gateway, websocket-service |
| gochat_ws_room_lease_acquire_total | counter | status | websocket-service |
| gochat_ws_room_lease_renew_total | counter | status | websocket-service |
| gochat_ws_room_handoff_total | counter | status | websocket-service |
| gochat_ws_room_handoff_duration_seconds | histogram | status | websocket-service |
| gochat_ws_sequence_conflict_total | counter | - | websocket-service |

### System

| 메트릭 | 타입 | 라벨 | 서비스 |
|--------|------|------|--------|
| gochat_build_info | gauge | goversion, vcs_revision, vcs_time, vcs_modified | 전체 |
| gochat_panic_recovered_total | counter | - | 앱 서비스 |
| container_cpu_usage_seconds_total | counter | namespace, pod, container, node | K8s cAdvisor |
| container_memory_working_set_bytes | gauge | namespace, pod, container, node | K8s cAdvisor |
| kube_pod_info | gauge | namespace, pod, node | kube-state-metrics |
| kube_pod_container_status_restarts_total | counter | namespace, pod, container | kube-state-metrics |
| kube_deployment_status_replicas_ready | gauge | namespace, deployment | kube-state-metrics |
| kube_horizontalpodautoscaler_status_current_replicas | gauge | namespace, horizontalpodautoscaler | kube-state-metrics |

### Go Runtime

- 수집 규칙: `go.opentelemetry.io/contrib/instrumentation/runtime v0.67.0`
- 서비스: 전체
- 기본값에서는 deprecated runtime metric을 켜지 않는다. 따라서 dashboard는 OTel semantic metric 이름을 Prometheus 변환 형태로 사용한다.

#### Runtime

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| go_goroutine_count | updowncounter | namespace, service, pod, node, component |
| go_processor_limit | updowncounter | namespace, service, pod, node, component |
| go_config_gogc | updowncounter | namespace, service, pod, node, component |
| go_memory_used | updowncounter | namespace, service, pod, node, component, go_memory_type |
| go_memory_limit | updowncounter | namespace, service, pod, node, component |
| go_memory_allocated_total | counter | namespace, service, pod, node, component |
| go_memory_allocations_total | counter | namespace, service, pod, node, component |
| go_memory_gc_goal | updowncounter | namespace, service, pod, node, component |

---

## 6. Traces

샘플링: `ParentBased(TraceIDRatioBased(0.1))`

### Auto

| 계층 | 라이브러리 | 서비스 |
|------|-----------|--------|
| HTTP 서버 | otelhttp.NewMiddleware | api-gateway, ws-gateway, websocket-service |
| gRPC 서버 | otelgrpc.NewServerHandler | user-service, chat-service |
| gRPC 클라이언트 | otelgrpc.NewClientHandler | api-gateway, websocket-service |
| Redis 클라이언트 | redisotel.InstrumentTracing | api-gateway, ws-gateway |

### Manual

| 스팬 | 어트리뷰트 | 서비스 |
|------|-----------|--------|
| pg.{SELECT,INSERT,UPDATE,DELETE} | db.system, db.operation | user-service |
| mongo.{InsertMany,Find,FindOne} | db.system, db.operation | chat-service |

### Tempo Metrics Generator

Tempo가 트레이스 데이터로부터 서비스 그래프 메트릭을 생성하여 Prometheus에 remote write.

| 프로세서 | 용도 | 디멘션 |
|----------|------|--------|
| service-graphs | 서비스 맵 시각화 | service.name |
| local-blocks | Tempo 내부 검색 최적화 | - |

---

## 7. Profiles

| 프로파일 | 설명 |
|----------|------|
| CPU | CPU 사용 플레임그래프 |
| InuseObjects | 라이브 오브젝트 수 |
| InuseSpace | 라이브 메모리 |
| Goroutines | 고루틴 생성 스택트레이스 |

- 서비스: 전체
