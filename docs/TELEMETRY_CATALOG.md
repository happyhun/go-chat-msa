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
| Logs | Loki | Alloy가 Docker API로 컨테이너 로그 스트리밍 수신 → Loki HTTP push | 1s (Alloy 기본값) | 7일 |
| Metrics | Prometheus | OTel SDK가 Alloy로 OTLP HTTP push → Alloy가 Prometheus remote write | 15s | 7일 |
| Traces | Tempo | SDK가 Alloy로 OTLP HTTP push → Alloy가 Tempo로 OTLP HTTP push | 5s (SDK 기본값) | 7일 |
| Profiles | Pyroscope | SDK가 Pyroscope로 HTTP push | 15s (SDK 기본값) | 7일 |

---

## 2. 서비스별 요약

| 서비스 | Logs | Metrics | Traces |
|--------|------|---------|--------|
| api-gateway | HTTP | HTTP, gRPC Client | HTTP, gRPC Client |
| ws-gateway | HTTP | HTTP, Routing | HTTP |
| websocket-service | HTTP | HTTP, WebSocket, Persistence | HTTP, gRPC Client |
| user-service | gRPC | gRPC Server, PostgreSQL, Domain | gRPC Server, PostgreSQL |
| chat-service | gRPC | gRPC Server, MongoDB, Domain | gRPC Server, MongoDB |
| retention-worker | Cron | Retention | - |

---

## 3. 헬스체크 필터링

세 가지 시그널 모두에 적용.

### 서비스 레벨

| 대상 | 필터 |
|------|------|
| HTTP 메트릭/로그 | HTTPMetricsMiddleware, LoggingMiddleware가 `/health` 스킵 |
| gRPC 메트릭/로그 | UnaryServerInterceptor, UnaryLoggingInterceptor가 `grpc.health.v1.Health/*` 스킵 |
| gRPC 클라이언트 메트릭 | UnaryClientInterceptor가 `grpc.health.v1.Health/*` 스킵 |

### Alloy 레벨

| 시그널 | 필터 |
|--------|------|
| Logs | `/health`, `grpc.health.v1.Health/Check`, `HealthCheck`, `pg_isready`, `adminCommand: ping` 드롭 |
| Traces | `/health`, `grpc.health.*`, `HealthCheck` 스팬 드롭 |

---

## 4. Logs

### Common

| 필드 | 설명 |
|------|------|
| level | info, warn, error, debug |
| time | RFC3339Nano |
| msg | 로그 메시지 |
| service | 서비스명 (Docker label) |
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

- 서비스: websocket-service

### Persistence

| 메트릭 | 타입 | 라벨 |
|--------|------|------|
| gochat_ws_persist_channel_depth | gauge | - |
| gochat_ws_persist_dropped_total | counter | - |
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

### Domain

| 메트릭 | 타입 | 라벨 | 서비스 |
|--------|------|------|--------|
| gochat_user_created_total | counter | status | user-service |
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
| gochat_retention_duration_seconds | histogram | - | retention-worker |
| gochat_retention_purged_total | counter | status | retention-worker |

### System

| 메트릭 | 타입 | 라벨 | 서비스 |
|--------|------|------|--------|
| gochat_build_info | gauge | goversion, vcs_revision, vcs_time, vcs_modified | 전체 |
| gochat_panic_recovered_total | counter | - | retention-worker 제외 |
| container_cpu_usage_seconds_total | counter | service, cpu | cAdvisor |
| container_memory_rss | gauge | service | cAdvisor |

### Go Runtime

- 수집 규칙: `GoRuntimeMetricsRule` regexp `/gc/`, `/sched/`, `/memory/classes/` + 기본 Go collector
- 서비스: 전체

#### GC

| 메트릭 | 타입 |
|--------|------|
| go_gc_duration_seconds | summary |
| go_gc_pauses_seconds | histogram |
| go_gc_cycles_automatic_gc_cycles_total | counter |
| go_gc_cycles_forced_gc_cycles_total | counter |
| go_gc_cycles_total_gc_cycles_total | counter |
| go_gc_heap_allocs_by_size_bytes | histogram |
| go_gc_heap_allocs_bytes_total | counter |
| go_gc_heap_allocs_objects_total | counter |
| go_gc_heap_frees_by_size_bytes | histogram |
| go_gc_heap_frees_bytes_total | counter |
| go_gc_heap_frees_objects_total | counter |
| go_gc_heap_goal_bytes | gauge |
| go_gc_heap_live_bytes | gauge |
| go_gc_heap_objects_objects | gauge |
| go_gc_heap_tiny_allocs_objects_total | counter |
| go_gc_gogc_percent | gauge |
| go_gc_gomemlimit_bytes | gauge |
| go_gc_limiter_last_enabled_gc_cycle | gauge |
| go_gc_scan_globals_bytes | gauge |
| go_gc_scan_heap_bytes | gauge |
| go_gc_scan_stack_bytes | gauge |
| go_gc_scan_total_bytes | gauge |
| go_gc_stack_starting_size_bytes | gauge |
| go_gc_cleanups_executed_cleanups_total | counter |
| go_gc_cleanups_queued_cleanups_total | counter |
| go_gc_finalizers_executed_finalizers_total | counter |
| go_gc_finalizers_queued_finalizers_total | counter |

#### Scheduler

| 메트릭 | 타입 |
|--------|------|
| go_goroutines | gauge |
| go_threads | gauge |
| go_sched_gomaxprocs_threads | gauge |
| go_sched_goroutines_goroutines | gauge |
| go_sched_goroutines_created_goroutines_total | counter |
| go_sched_goroutines_runnable_goroutines | gauge |
| go_sched_goroutines_running_goroutines | gauge |
| go_sched_goroutines_waiting_goroutines | gauge |
| go_sched_goroutines_not_in_go_goroutines | gauge |
| go_sched_latencies_seconds | histogram |
| go_sched_pauses_stopping_gc_seconds | histogram |
| go_sched_pauses_stopping_other_seconds | histogram |
| go_sched_pauses_total_gc_seconds | histogram |
| go_sched_pauses_total_other_seconds | histogram |
| go_sched_threads_total_threads | gauge |

#### Memory

| 메트릭 | 타입 |
|--------|------|
| go_memstats_alloc_bytes | gauge |
| go_memstats_alloc_bytes_total | counter |
| go_memstats_sys_bytes | gauge |
| go_memstats_heap_alloc_bytes | gauge |
| go_memstats_heap_idle_bytes | gauge |
| go_memstats_heap_inuse_bytes | gauge |
| go_memstats_heap_objects | gauge |
| go_memstats_heap_released_bytes | gauge |
| go_memstats_heap_sys_bytes | gauge |
| go_memstats_stack_inuse_bytes | gauge |
| go_memstats_stack_sys_bytes | gauge |
| go_memstats_mspan_inuse_bytes | gauge |
| go_memstats_mspan_sys_bytes | gauge |
| go_memstats_mcache_inuse_bytes | gauge |
| go_memstats_mcache_sys_bytes | gauge |
| go_memstats_buck_hash_sys_bytes | gauge |
| go_memstats_gc_sys_bytes | gauge |
| go_memstats_other_sys_bytes | gauge |
| go_memstats_next_gc_bytes | gauge |
| go_memstats_last_gc_time_seconds | gauge |
| go_memstats_mallocs_total | counter |
| go_memstats_frees_total | counter |
| go_memory_classes_total_bytes | gauge |
| go_memory_classes_heap_free_bytes | gauge |
| go_memory_classes_heap_objects_bytes | gauge |
| go_memory_classes_heap_released_bytes | gauge |
| go_memory_classes_heap_stacks_bytes | gauge |
| go_memory_classes_heap_unused_bytes | gauge |
| go_memory_classes_metadata_mcache_free_bytes | gauge |
| go_memory_classes_metadata_mcache_inuse_bytes | gauge |
| go_memory_classes_metadata_mspan_free_bytes | gauge |
| go_memory_classes_metadata_mspan_inuse_bytes | gauge |
| go_memory_classes_metadata_other_bytes | gauge |
| go_memory_classes_os_stacks_bytes | gauge |
| go_memory_classes_other_bytes | gauge |
| go_memory_classes_profiling_buckets_bytes | gauge |

#### Other

| 메트릭 | 타입 |
|--------|------|
| go_info | gauge |

---

## 6. Traces

샘플링: `ParentBased(TraceIDRatioBased(0.1))`

### Auto

| 계층 | 라이브러리 | 서비스 |
|------|-----------|--------|
| HTTP 서버 | otelhttp.NewMiddleware | api-gateway, ws-gateway, websocket-service |
| gRPC 서버 | otelgrpc.NewServerHandler | user-service, chat-service |
| gRPC 클라이언트 | otelgrpc.NewClientHandler | api-gateway, websocket-service |

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
