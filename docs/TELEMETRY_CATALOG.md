# 텔레메트리 카탈로그

이 문서는 현재 mainline 프로젝트 기준의 텔레메트리 카탈로그다. 기준 소스는 앱 계측 코드(`cmd/*`, `internal/shared/telemetry`, `internal/shared/middleware`, 각 도메인 `metrics.go`), Kubernetes 관측성 설정(`deploy/k8s/base/observability/config/alloy/config.alloy`, `deploy/k8s/base/observability/observability.yaml`), backend 설정(`observability/*/config.yaml`), QA HPA/Prometheus Adapter 설정, Grafana dashboard query다.

## 목차

1. [개요](#1-개요)
2. [서비스별 요약](#2-서비스별-요약)
3. [헬스체크 필터링](#3-헬스체크-필터링)
4. [Logs](#4-logs)
5. [Metrics](#5-metrics)
6. [Traces](#6-traces)
7. [Profiles](#7-profiles)
8. [Kubernetes HPA Metric](#8-kubernetes-hpa-metric)

---

## 1. 개요

현재 실행 경로는 Kubernetes overlay 기준이다. Docker Compose manifest는 mainline에서 제거되어 있고, 루트의 `observability/alloy/config.alloy`는 Docker discovery 기반 legacy/local 설정으로 남아 있다.

Grafana Full Stack 기반 관측성 구성이다. Metrics/Traces는 앱 SDK가 Alloy로 OTLP HTTP push하고, Logs는 Alloy가 Pod 로그를 tailing해서 Loki로 push한다. Profiles는 Alloy를 거치지 않고 앱 SDK가 Pyroscope로 직접 push한다.

| 시그널 | 백엔드 | 현재 수집 경로 | 주기 | 보존 |
|--------|--------|----------------|------|------|
| Logs | Loki | Alloy `loki.source.kubernetes` → Loki HTTP push | 스트리밍 tailing | 168h |
| Metrics | Prometheus | 앱 OTLP HTTP push → Alloy Prometheus exporter/remote write, Alloy cAdvisor/kube-state-metrics scrape → remote write | 앱/infra 15s | 7d |
| Traces | Tempo | 앱 OTLP HTTP push → Alloy filter/batch → Tempo OTLP HTTP | SDK batch 기본 5s | 168h |
| Profiles | Pyroscope | 앱 Pyroscope SDK → Pyroscope HTTP push | pyroscope-go 기본 15s | 168h |

앱은 `OTEL_ENDPOINT`가 비어 있으면 OTel 초기화를 건너뛰고, `PYROSCOPE_ENDPOINT`가 비어 있으면 Pyroscope 초기화를 건너뛴다. 현재 dev/test/qa 앱 overlay는 `OTEL_ENDPOINT="alloy:4318"`, `PYROSCOPE_ENDPOINT="http://pyroscope:4040"`를 설정한다.

공통 OTel resource에는 `resource.WithFromEnv()`, `resource.WithProcess()`, `resource.WithOS()`, `resource.WithHost()`로 얻은 표준 resource attribute와 `service.name`, `service.instance.id`가 들어간다. `service.instance.id`는 K8s `POD_NAME`을 우선 사용하고, 없으면 hostname, hostname도 없으면 UUID로 fallback한다.

K8s Alloy는 OTLP metric datapoint에 다음 resource 정보를 라벨로 복사한다: `namespace`, `deployment`, `pod`, `container`, `node`, `service`, `service_instance_id`, `component`. 아래 Metrics 표의 "계측 라벨"은 코드 또는 라이브러리가 직접 붙이는 라벨이며, K8s 공통 라벨은 별도 표기하지 않는다.

---

## 2. 서비스별 요약

| 서비스 | Logs | Metrics | Traces |
|--------|------|---------|--------|
| api-gateway | HTTP request log | HTTP, gRPC client, Redis, recovery, build/runtime | HTTP server, gRPC client, Redis |
| ws-gateway | HTTP request log | HTTP, routing, membership, Redis, recovery, build/runtime | HTTP server, Redis |
| websocket-service | HTTP request log | HTTP, WebSocket hub/session, persistence, room lease/handoff, membership, gRPC client, Redis, recovery, build/runtime | HTTP server, gRPC client, Redis |
| user-service | gRPC request log | gRPC server, PostgreSQL, pgxpool, Redis, user/auth/room, hasher, recovery, build/runtime | gRPC server, PostgreSQL, Redis |
| chat-service | gRPC request log | gRPC server, MongoDB, Mongo pool, chat domain, recovery, build/runtime | gRPC server, MongoDB |

Frontend, Swagger UI, Redis/Postgres/Mongo, load-test Pod 로그도 Alloy 수집 대상이 될 수 있지만, `postgres-migrate`, `mongo-migrate` Pod 로그는 K8s Alloy relabel 단계에서 drop된다. 이 카탈로그의 앱 텔레메트리 항목은 위 5개 Go 서비스 기준이다.

---

## 3. 헬스체크 필터링

헬스체크 노이즈는 서비스 계층과 Alloy 계층에서 분리해 제거한다.

### 서비스 레벨

| 대상 | 필터 |
|------|------|
| HTTP 메트릭/로그 | `telemetry.MetricsMiddleware`, `middleware.LoggingMiddleware`가 `/health`, `/ready` 스킵 |
| gRPC 서버 메트릭/로그 | `telemetry.MetricsServerInterceptor`, `middleware.UnaryLoggingInterceptor`가 `/grpc.health.v1.Health/*` 스킵 |
| gRPC 클라이언트 메트릭 | `telemetry.MetricsClientInterceptor`가 `/grpc.health.v1.Health/*` 스킵 |

HTTP/gRPC OTel auto instrumentation은 health request span을 만들 수 있으므로 trace 필터링은 Alloy에서 한 번 더 수행한다.

### Alloy 레벨

| 시그널 | 필터 |
|--------|------|
| Logs | `/health`, `/ready`, `grpc.health.v1.Health/Check`, `HealthCheck`, `pg_isready`, `adminCommand: ping` 포함 로그 드롭 |
| Traces | `/health`, `/ready`, `grpc.health.*`, `HealthCheck` span 드롭 |

---

## 4. Logs

앱은 `slog.NewJSONHandler`에 `AddSource: true`를 켜서 stdout에 JSON 로그를 쓴다. `TraceHandler`는 현재 context에 유효한 span이 있을 때만 `trace_id`, `span_id`를 JSON 필드로 추가한다.

### 앱 JSON 필드

| 필드 | 설명 |
|------|------|
| time | RFC3339Nano |
| level | `DEBUG`, `INFO`, `WARN`, `ERROR`가 JSON에 기록되고 Alloy가 label로는 lowercase 변환 |
| msg | 로그 메시지 |
| source | `function`, `file`, `line`을 담는 slog source object |
| trace_id | 유효한 OTel span context가 있을 때만 기록 |
| span_id | 유효한 OTel span context가 있을 때만 기록 |

### Loki 라벨

| 라벨 | 출처 |
|------|------|
| service | K8s `app.kubernetes.io/name` |
| component | K8s `app.kubernetes.io/component` |
| namespace | K8s namespace |
| pod | K8s Pod name |
| container | K8s container name |
| node | K8s node name |
| level | Go/Mongo/Postgres log parser가 추출 |
| trace_id | Go 앱 JSON 로그에서 추출 |

K8s Alloy는 Go 앱 로그에서 `span_id`를 Loki label로 승격하지 않는다. `span_id`는 원본 JSON 로그 필드로만 남는다.

### HTTP Request Log

| 필드 | 설명 |
|------|------|
| method | HTTP 메서드 |
| path | 요청 경로 |
| status | 응답 코드 |
| latency_ms | 처리 시간(ms) |
| bytes_written | 응답 바이트 |
| content_length | 요청 body 크기 |
| remote_addr | 클라이언트 주소 |
| user_agent | User-Agent |
| query | 쿼리 파라미터가 있을 때만 기록 |
| xff | `X-Forwarded-For`가 있을 때만 기록 |
| hijacked | WebSocket upgrade처럼 connection hijack이 발생한 경우만 기록 |

- 5xx 응답은 ERROR 레벨이다.
- `token`, `password`, `secret`, `key`, `authorization`, `access_token`, `refresh_token` 쿼리 파라미터 값은 `***`로 마스킹한다.
- 서비스: api-gateway, ws-gateway, websocket-service

### gRPC Request Log

| 필드 | 설명 |
|------|------|
| method | gRPC full method |
| code | gRPC status code |
| latency_ms | 처리 시간(ms) |
| error | 에러가 있을 때만 기록 |

- `Internal`, `Unknown`, `DataLoss`, `Unavailable`은 ERROR 레벨이다.
- 서비스: user-service, chat-service

### Recovery Log

HTTP/gRPC panic recovery는 `gochat_panic_recovered` counter를 증가시키고 ERROR 로그를 남긴다.

| 필드 | HTTP | gRPC |
|------|------|------|
| error | O | O |
| stack | O | O |
| method | HTTP method | gRPC full method |
| path | O | - |

---

## 5. Metrics

아래 이름은 Prometheus/Grafana에서 조회하는 이름 기준이다. OTel Counter는 코드 등록명에 `_total`이 없어도 Prometheus exporter 변환 후 `_total`로 노출된다. Histogram은 조회 시 `_bucket`, `_sum`, `_count` 시계열이 함께 생성된다. 이미 코드 등록명이 `_total`로 끝나는 일부 Counter는 Prometheus 조회명도 동일하게 `_total`로 표기한다.

### HTTP

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_http_requests_total | counter | service, method, path, status_code |
| gochat_http_request_duration_seconds | histogram | service, method, path, status_code |

- 서비스: api-gateway, ws-gateway, websocket-service
- `path`는 UUID path segment를 `:id`로 정규화한다.

### gRPC Server

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_grpc_requests_total | counter | service, method, code |
| gochat_grpc_request_duration_seconds | histogram | service, method, code |

- 서비스: user-service, chat-service

### gRPC Client

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_grpc_client_requests_total | counter | service, method, code |
| gochat_grpc_client_request_duration_seconds | histogram | service, method, code |

- 서비스: api-gateway, websocket-service

### WebSocket / Hub

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_ws_hubs_active | updowncounter | - |
| gochat_ws_hubs_closed_total | counter | reason |
| gochat_ws_connections_active | updowncounter | - |
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
- `gochat_ws_connections_active`는 QA HPA custom metric으로도 사용된다.
- `gochat_ws_hubs_active`, `gochat_ws_connections_active`는 OTel UpDownCounter라 Prometheus에서는 현재값 계열로 조회한다.

### Persistence

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
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
- 코드상 `gochat_persistence_retry_queue_full_total`은 등록되어 있지만 현재 Grafana dashboard query에는 직접 쓰이지 않는다.

### PostgreSQL

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
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
- `operation`은 SQL 첫 유효 line의 첫 키워드를 대문자로 추출한다. 현재 sqlc query 기준 주요 값은 `SELECT`, `INSERT`, `UPDATE`, `DELETE`다.

### MongoDB

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_mongo_query_duration_seconds | histogram | operation |
| gochat_mongo_query_total | counter | operation, status |
| gochat_mongo_pool_checked_out_conns | gauge | - |
| gochat_mongo_pool_open_conns | gauge | - |
| gochat_mongo_pool_created_total | counter | - |
| gochat_mongo_pool_closed_total | counter | - |

- 서비스: chat-service
- Mongo collection wrapper가 계측하는 `operation` 값은 `INSERT_MANY`, `FIND`, `FIND_ONE`이다.

### Redis

`redisotel.InstrumentMetrics`가 `db.client.connections.*` 계열 OTel semantic convention 메트릭을 자동 등록한다. Prometheus 조회명은 점이 `_`로 바뀌고 단위 suffix가 붙은 이름이다.

공통 계측 라벨은 `db_system="redis"`, `pool_name=<redis address>`다.

| 메트릭 | OTel 타입 | 추가 계측 라벨 |
|--------|-----------|----------------|
| db_client_connections_idle_max | observable updowncounter | - |
| db_client_connections_idle_min | observable updowncounter | - |
| db_client_connections_max | observable updowncounter | - |
| db_client_connections_usage | observable updowncounter | state |
| db_client_connections_waits | observable updowncounter | - |
| db_client_connections_waits_duration_nanoseconds | observable updowncounter | - |
| db_client_connections_timeouts | observable updowncounter | - |
| db_client_connections_hits | observable updowncounter | - |
| db_client_connections_misses | observable updowncounter | - |
| db_client_connections_create_time_milliseconds | histogram | status, error_type |
| db_client_connections_use_time_milliseconds | histogram | type, status, error_type |

- 서비스: api-gateway, ws-gateway, websocket-service, user-service
- `state` 값은 `idle`, `used`다.
- `type` 값은 `command`, `pipeline`이다.
- `status` 값은 `ok`, `nil`, `error`이고, `error_type` 값은 `none`, `context_canceled`, `context_timeout`, `other`다.
- 명령별 Redis operation latency는 metric label이 아니라 `redisotel.InstrumentTracing` span에서 확인한다.

### Application / Domain

| 메트릭 | 타입 | 계측 라벨 | 서비스 |
|--------|------|-----------|--------|
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

### System / Kubernetes

| 메트릭 | 타입 | 주요 라벨 | 출처 |
|--------|------|-----------|------|
| gochat_build_info | gauge | goversion, vcs_revision, vcs_time, vcs_modified | 앱 서비스 |
| gochat_panic_recovered_total | counter | - | 앱 서비스 |
| container_cpu_usage_seconds_total | counter | namespace, pod, container, node | cAdvisor |
| container_memory_working_set_bytes | gauge | namespace, pod, container, node | cAdvisor |
| container_memory_rss | gauge | namespace, pod, container, node | cAdvisor |
| kube_pod_info | gauge | namespace, pod, node | kube-state-metrics |
| kube_pod_status_phase | gauge | namespace, pod, phase | kube-state-metrics |
| kube_pod_container_status_restarts_total | counter | namespace, pod, container | kube-state-metrics |
| kube_deployment_spec_replicas | gauge | namespace, deployment | kube-state-metrics |
| kube_deployment_status_replicas | gauge | namespace, deployment | kube-state-metrics |
| kube_deployment_status_replicas_available | gauge | namespace, deployment | kube-state-metrics |
| kube_deployment_status_replicas_ready | gauge | namespace, deployment | kube-state-metrics |
| kube_horizontalpodautoscaler_status_current_replicas | gauge | namespace, horizontalpodautoscaler | kube-state-metrics |
| kube_horizontalpodautoscaler_status_desired_replicas | gauge | namespace, horizontalpodautoscaler | kube-state-metrics |
| kube_horizontalpodautoscaler_spec_min_replicas | gauge | namespace, horizontalpodautoscaler | kube-state-metrics |
| kube_horizontalpodautoscaler_spec_max_replicas | gauge | namespace, horizontalpodautoscaler | kube-state-metrics |

Alloy는 cAdvisor에서 위 container 메트릭만 keep하고, namespace/pod가 있는 실제 컨테이너만 남긴다. kube-state-metrics도 현재 namespace와 위 allowlist metric만 remote write한다.

### Go Runtime

- 수집 라이브러리: `go.opentelemetry.io/contrib/instrumentation/runtime v0.67.0`
- 앱 코드 호출: `runtime.Start(runtime.WithMinimumReadMemStatsInterval(15s))`
- 서비스: 전체 앱 서비스
- `OTEL_GO_X_DEPRECATED_RUNTIME_METRICS`를 켜지 않으므로 deprecated runtime metric은 기본 수집 대상이 아니다.

| 메트릭 | OTel 타입 | 계측 라벨 |
|--------|-----------|-----------|
| go_goroutine_count | observable updowncounter | - |
| go_processor_limit | observable updowncounter | - |
| go_config_gogc | observable updowncounter | - |
| go_memory_used | observable updowncounter | go_memory_type |
| go_memory_limit | observable updowncounter | - |
| go_memory_allocated_total | observable counter | - |
| go_memory_allocations_total | observable counter | - |
| go_memory_gc_goal | observable updowncounter | - |

K8s 공통 라벨(`namespace`, `service`, `pod`, `node`, `component` 등)은 Alloy transform으로 추가된다. `go_memory_type`은 OTel attribute `go.memory.type`이 Prometheus label로 변환된 이름이며, 현재 runtime dashboard는 `stack`, `other`를 조회한다.

---

## 6. Traces

샘플링은 앱 SDK에서 `ParentBased(TraceIDRatioBased(0.1))`로 설정한다. Exporter는 OTLP HTTP로 Alloy `:4318`에 전송하고, `WithBatcher`의 batch timeout은 코드에서 override하지 않아 SDK 기본값 5s를 사용한다.

### Auto Instrumentation

| 계층 | 라이브러리/설정 | 서비스 |
|------|----------------|--------|
| HTTP server | `otelhttp.NewMiddleware` | api-gateway, ws-gateway, websocket-service |
| gRPC server | `otelgrpc.NewServerHandler` | user-service, chat-service |
| gRPC client | `otelgrpc.NewClientHandler` | api-gateway, websocket-service |
| Redis client | `redisotel.InstrumentTracing` | api-gateway, ws-gateway, websocket-service, user-service |

HTTP span name은 `METHOD + " " + telemetry.NormalizePath(path)` 형태다. UUID path segment는 `:id`로 정규화된다.

### Manual Spans

| 스팬 | 어트리뷰트 | 서비스 |
|------|-----------|--------|
| pg.`<SQL_OP>` | db.system=`postgresql`, db.operation=`<SQL_OP>` | user-service |
| mongo.InsertMany | db.system=`mongodb`, db.operation=`InsertMany` | chat-service |
| mongo.Find | db.system=`mongodb`, db.operation=`Find` | chat-service |
| mongo.FindOne | db.system=`mongodb`, db.operation=`FindOne` | chat-service |

PostgreSQL `<SQL_OP>`는 SQL 첫 유효 line의 첫 키워드다. 현재 query 기준 주요 값은 `SELECT`, `INSERT`, `UPDATE`, `DELETE`다.

### Tempo Metrics Generator

Tempo가 trace 데이터에서 service graph 계열 메트릭을 만들어 Prometheus에 remote write한다.

| 프로세서 | 용도 | 설정 |
|----------|------|------|
| service-graphs | 서비스 맵 시각화 | dimension `service.name` |
| local-blocks | Tempo 내부 검색 최적화 | `flush_to_storage: true` |

---

## 7. Profiles

모든 앱 서비스는 `PYROSCOPE_ENDPOINT`가 비어 있지 않을 때 Pyroscope profiler를 시작한다.

| 프로파일 | 설명 |
|----------|------|
| CPU | CPU 사용 플레임그래프 |
| InuseObjects | live object 수 |
| InuseSpace | live heap memory |
| Goroutines | goroutine stack profile |

- 서비스: api-gateway, ws-gateway, websocket-service, user-service, chat-service
- 업로드 주기: pyroscope-go 기본 15s
- Mutex/Block profile은 현재 수집하지 않으며, 관련 Go runtime profiler도 활성화하지 않는다.

---

## 8. Kubernetes HPA Metric

`qa` overlay는 `websocket-service` HPA를 CPU가 아니라 WebSocket active connection 수로 검증한다.

```text
websocket-service
  -> OTel metric gochat_ws_connections_active
  -> Alloy OTLP receiver
  -> Alloy k8sattributes + metric label transform
  -> Prometheus remote write
  -> Prometheus Adapter
  -> custom.metrics.k8s.io
  -> HPA websocket-service
```

HPA가 읽는 metric:

| 항목 | 값 |
|------|-----|
| Kubernetes API | `custom.metrics.k8s.io/v1beta1` |
| Metric name | `gochat_ws_connections_active` |
| Metric type | Pods metric |
| Target | `AverageValue: 100` |
| HPA 대상 | `deployment/websocket-service` |
| QA replica 범위 | `minReplicas=1`, `maxReplicas=2` |
| Scale-up 정책 | stabilization `0s`, `+1 pod / 30s` |
| Scale-down 정책 | stabilization `300s`, `-1 pod / 60s` |

Prometheus Adapter rule:

| 항목 | 값 |
|------|-----|
| seriesQuery | `gochat_ws_connections_active{namespace!="",pod!=""}` |
| name.matches | `^gochat_ws_connections_active$` |
| name.as | `gochat_ws_connections_active` |
| metricsQuery | `sum(<<.Series>>{<<.LabelMatchers>>}) by (<<.GroupBy>>)` |
| relist/max-age | `15s` / `2m` |

따라서 Alloy/Prometheus 경로에서 `namespace`와 `pod` 라벨이 유지되어야 HPA가 Pod metric으로 해석할 수 있다. 이 값은 운영 autoscaling 기준이 아니라 로컬 kind QA에서 HPA handoff를 재현하기 위한 낮은 기준이다. 성능 판단은 `dev-load` C10K 시나리오, HPA handoff 정합성 판단은 `qa-load` 시나리오로 분리한다.
