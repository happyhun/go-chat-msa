# 텔레메트리 카탈로그

Go Chat MSA에서 **무엇을 수집하고, 어디에서 조회하며, 어떻게 해석하는지** 정리한 참조 문서다. Kubernetes에서 실행하는 네 Go 서비스와 인프라를 대상으로 한다.

## 목차

1. [한눈에 보기](#1-한눈에-보기)
2. [로그](#2-로그)
3. [메트릭](#3-메트릭)
4. [트레이스](#4-트레이스)
5. [프로파일](#5-프로파일)
6. [수집 조건과 필터](#6-수집-조건과-필터)
7. [관련 소스](#7-관련-소스)

## 1. 한눈에 보기

### 목적별 조회 안내

| 확인할 내용 | 시작점 | 상세 정보 |
|-------------|--------|-----------|
| 서비스 전체 상태 | Grafana · Operations Overview | 요청 오류·지연, 저장 대기량 |
| HTTP/gRPC 요청량과 응답 지연 | Grafana · API Traffic | [요청 처리 메트릭](#요청-처리) |
| WebSocket 연결과 메시지 전달 | Grafana · Realtime Messaging | [WebSocket 메시지 전달](#websocket-메시지-전달), [NATS 서버](#nats-서버) |
| 메시지 저장 지연과 적체 | Grafana · Data Persistence | [메시지 영속화](#메시지-영속화) |
| Pod·컨테이너와 Go 런타임 상태 | Grafana · Platform Runtime | [시스템과 Kubernetes](#시스템과-kubernetes), [Go 런타임](#go-런타임) |
| 개별 요청의 오류와 진단 메시지 | Loki | [로그 필드와 라벨](#2-로그) |
| 서비스 간 호출과 DB 작업 | Tempo | [트레이스 범위와 샘플링](#4-트레이스) |
| 함수별 CPU·메모리 사용량 | Pyroscope | [수집 프로파일](#5-프로파일) |

### 수집 흐름

Grafana에서 로그·메트릭·트레이스·프로파일을 조회한다. Alloy는 로그 수집과 메트릭·트레이스 전달을 담당한다.

| 데이터 | 저장소 | 수집·전송 주기 | 보존 기간 |
|--------|--------|----------------|-----------|
| 로그 | Loki | Kubernetes API로 Pod 로그 스트리밍 수집 | 7일 |
| 앱·인프라 메트릭 | Prometheus | 앱 전송·인프라 스크레이프 15초 | 7일 |
| 트레이스 | Tempo | 앱 SDK 배치 timeout 기본 5초 | 7일 |
| 프로파일 | Pyroscope | 앱 SDK 직접 전송, 기본 15초 | 7일 |

### 서비스별 수집 범위

| 서비스 | Logs | Metrics | Traces |
|--------|------|---------|--------|
| api-gateway | HTTP 요청 | HTTP, gRPC client, Redis, recovery, build/runtime | HTTP server, gRPC client, Redis |
| websocket-service | HTTP 요청 | HTTP, WebSocket hub/session, NATS, JetStream publish, gRPC client, Redis, recovery, build/runtime | HTTP server, gRPC client, Redis |
| user-service | gRPC 요청 | gRPC server, PostgreSQL, pgxpool, Redis, user/auth/room, hasher, recovery, build/runtime | gRPC server, PostgreSQL, Redis |
| chat-service | gRPC 요청 | gRPC server, MongoDB, Mongo pool, JetStream persistence, chat domain, recovery, build/runtime | gRPC server, MongoDB |

네 Go 서비스 모두 CPU·메모리·goroutine 프로파일을 수집한다. Frontend, Swagger UI, 데이터 서비스, 부하 테스트 Pod 로그도 Alloy 수집 대상이며, 수집 제외 규칙은 [헬스체크와 로그 필터](#헬스체크와-로그-필터)에 정리한다.

### 공통 라벨과 식별자

서비스 단위 조회에는 `service`, 배포 환경 구분에는 `namespace`, 인스턴스 단위 조회에는 `pod`를 사용한다.

| 메트릭 공통 라벨 | 의미 |
|------------------|------|
| `namespace`, `deployment` | Kubernetes namespace와 Deployment |
| `pod`, `container`, `node` | 실행 위치 |
| `service`, `component` | 서비스 이름과 구성요소 분류 |
| `service_instance_id` | 앱 인스턴스 식별자 |

Alloy는 사용 가능한 Kubernetes·OTel resource 속성을 메트릭 라벨로 복사한다. `service_instance_id`는 `POD_NAME` → hostname → UUID 순서로 결정한다. 로그의 라벨 목록은 [Loki 라벨](#loki-라벨)을 참고한다.

**조회 전 확인할 세 가지**

- HTTP/gRPC 헬스체크는 커스텀 요청 메트릭에서 제외되지만 자동 계측 메트릭에는 포함된다.
- WebSocket 전달·저장 경로는 NATS 경계에서 트레이스가 이어지지 않으므로 전용 메트릭으로 확인한다.
- 저장 대기량은 여러 Pod가 같은 shared consumer를 관측하므로 Pod 값을 단순 합산하지 않는다.

## 2. 로그

앱 로그는 표준 출력에 기록되는 JSON이며, Loki에서 서비스·Pod·로그 레벨로 조회한다. 요청 로그 외에 panic 복구, 연결 오류, 저장 재시도 등의 진단 로그도 같은 경로로 수집된다.

### 공통 JSON 필드

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

`trace_id`와 `span_id`는 Loki 라벨이 아닌 JSON 필드다. 특정 트레이스의 로그는 JSON 파싱 후 필터링한다.

```logql
{service="api-gateway"} | json | trace_id="<trace ID>"
```

두 필드는 유효한 스팬 컨텍스트가 있는 로그에만 기록된다.

### HTTP 요청 로그

| 필드 | 설명 |
|------|------|
| method | HTTP 메서드 |
| path | 요청 경로 |
| status | 응답 코드. WebSocket 전환 등 연결 hijack 시 생략 |
| latency_ms | 처리 시간(ms). WebSocket 전환 등 연결 hijack 시 생략 |
| bytes_written | 응답 바이트. WebSocket 전환 등 연결 hijack 시 생략 |
| content_length | 요청 body 크기 |
| remote_addr | 클라이언트 주소 |
| user_agent | User-Agent |
| query | 쿼리 파라미터가 있을 때만 기록 |
| xff | `X-Forwarded-For`가 있을 때만 기록 |
| hijacked | WebSocket upgrade처럼 connection hijack이 발생한 경우만 기록 |

- 5xx 응답은 ERROR 레벨이다.
- WebSocket 연결로 전환된 요청은 `hijacked=true`를 INFO 레벨로 기록한다.
- `token`, `password`, `secret`, `key`, `authorization`, `access_token`, `refresh_token` 쿼리 파라미터 값은 `***`로 마스킹한다.
- `ticket`은 마스킹 대상에 포함되지 않아 WebSocket 요청의 `query`에 남을 수 있다.
- 서비스: api-gateway, websocket-service

### gRPC 요청 로그

| 필드 | 설명 |
|------|------|
| method | gRPC full method |
| code | gRPC status code |
| latency_ms | 처리 시간(ms) |
| error | 에러가 있을 때만 기록 |

- `Internal`, `Unknown`, `DataLoss`, `Unavailable`은 ERROR 레벨이다.
- 서비스: user-service, chat-service

### Panic 복구 로그

HTTP/gRPC 처리 중 panic을 복구하면 ERROR 로그와 `gochat_panic_recovered_total` 메트릭을 기록한다.

| 필드 | HTTP | gRPC |
|------|------|------|
| error | O | O |
| stack | O | O |
| method | HTTP method | gRPC full method |
| path | O | - |

## 3. 메트릭

메트릭 이름은 **Prometheus 조회명** 기준이다. 표의 계측 라벨은 앱이나 라이브러리가 붙이는 속성이며, [공통 라벨](#공통-라벨과-식별자)은 생략한다.

| 표기 | 의미와 조회 방법 |
|------|-----------------|
| counter | 누적 횟수·양. 증가율은 `rate()`, 구간 증가량은 `increase()`로 조회 |
| gauge / updowncounter | 현재값. OTel updowncounter도 Prometheus에서는 gauge로 조회 |
| histogram | 값의 분포. `_bucket`, `_sum`, `_count` 시계열로 조회 |
| observable | SDK가 수집 시점에 콜백으로 값을 읽는 OTel 계측 방식 |

Counter에는 `_total`, 단위에는 `_seconds`, `_bytes`, `_percent` 등의 접미사가 붙는다. 코드 등록명에 이미 있는 접미사는 중복해서 붙이지 않는다.

### 요청 처리

#### HTTP

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_http_requests_total | counter | service, method, path, status_code |
| gochat_http_request_duration_seconds | histogram | service, method, path, status_code |

- 서비스: api-gateway, websocket-service
- `path`는 UUID path segment를 `:id`로 정규화한다.
- WebSocket upgrade 성공처럼 connection hijack이 발생한 요청은 위 counter와 histogram에 기록하지 않는다. HTTP 자동 계측 메트릭은 별도 경로에서 기록한다.

#### gRPC 서버

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_grpc_requests_total | counter | service, method, code |
| gochat_grpc_request_duration_seconds | histogram | service, method, code |

- 서비스: user-service, chat-service

#### gRPC 클라이언트

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_grpc_client_requests_total | counter | service, method, code |
| gochat_grpc_client_request_duration_seconds | histogram | service, method, code |

- 서비스: api-gateway, websocket-service

#### HTTP / gRPC 자동 계측

`otelhttp`와 `otelgrpc`가 수집하는 표준 메트릭이다. 두 라이브러리의 버전은 `v0.71.0`이며, gRPC는 stable semantic convention을 사용한다(`OTEL_SEMCONV_STABILITY_OPT_IN` 미설정).

| 메트릭 | 타입 | 서비스 |
|--------|------|--------|
| http_server_request_duration_seconds | histogram | api-gateway, websocket-service |
| http_server_request_body_size_bytes | histogram | api-gateway, websocket-service |
| http_server_response_body_size_bytes | histogram | api-gateway, websocket-service |
| rpc_server_call_duration_seconds | histogram | user-service, chat-service |
| rpc_client_call_duration_seconds | histogram | api-gateway, websocket-service |

- HTTP 계측 라벨은 `http_request_method`, `url_scheme`, `server_address`이며, 요청·응답 정보에 따라 `server_port`, `network_protocol_name`, `network_protocol_version`, `http_response_status_code`, `http_route`가 추가된다. 커스텀 메트릭의 `path` 정규화와 span name formatter가 자동 메트릭의 `http_route`를 설정하는 것은 아니다.
- gRPC 계측 라벨은 `rpc_system_name`, `rpc_method`, `rpc_response_status_code`이며, 라이브러리가 오류로 분류한 호출에는 `error_type`도 추가된다. 서버와 클라이언트의 오류 분류는 다르다.
- 커스텀 `gochat_*`와 별도 시계열이며 health request도 포함한다. 같은 요청을 관측하므로 두 계열을 합산하지 않는다.

### WebSocket 메시지 전달

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_ws_hubs_active | updowncounter | - |
| gochat_ws_hubs_closed_total | counter | reason |
| gochat_ws_connections_active | updowncounter | - |
| gochat_ws_sessions_closed_total | counter | reason |
| gochat_ws_messages_received_total | counter | - |
| gochat_ws_messages_rate_limited_total | counter | - |
| gochat_ws_messages_sent_total | counter | - |
| gochat_ws_send_queue_overflows_total | counter | - |
| gochat_ws_broadcast_channel_depth | histogram | - |
| gochat_ws_egress_duration_seconds | histogram | - |
| gochat_ws_broker_hop_duration_seconds | histogram | - |
| gochat_ws_jetstream_publish_ack_duration_seconds | histogram | status |
| gochat_ws_hub_fanout_duration_seconds | histogram | - |
| gochat_ws_out_of_order_total | counter | - |
| gochat_ws_reorder_span_seconds | histogram | - |
| gochat_ws_nats_slow_consumer_total | counter | - |
| gochat_ws_nats_dropped_messages_total | counter | - |
| gochat_ws_nats_publish_failed_total | counter | subject_kind |
| gochat_ws_nats_disconnects_total | counter | - |
| gochat_ws_nats_invalid_headers_total | counter | - |
| gochat_ws_room_events_ignored_total | counter | reason |

서비스: **websocket-service**. `active` 계열은 현재 활성 Hub·연결 수다.

#### 지연 측정 구간

| 지표 | 시작 → 종료 | 해석 |
|------|-------------|------|
| egress | 서버의 채팅 수신 → 같은 발신자 ID 세션의 소켓 쓰기 직전 | 소켓 쓰기 완료 시간 제외. 다른 Pod의 동일 사용자 세션에서는 Pod 간 시계 차이 포함 가능 |
| broker hop | 메시지의 `ReceivedAt` → 구독 콜백 도착 | 송신 Pod의 수신 시각 기준. 발행 과정과 Pod 간 시계 차이가 포함될 수 있음 |
| publish ack | JetStream 발행 호출 → `PubAck` 또는 오류 반환 | `status=accepted`는 발행 수락이며 MongoDB 저장 완료를 뜻하지 않음 |
| hub fan-out | 구독 콜백 도착 → 로컬 세션 큐 적재 | 수신 Pod의 시계로 측정 |

#### 종료·누락 지표 해석

| 항목 | 의미 |
|------|------|
| send queue overflow | 세션당 최초 포화 시 한 번 기록. 같은 종료는 `sessions_closed`의 `reason=send_queue_overflow`에도 기록 |
| NATS dropped | slow-consumer 콜백마다 구독의 누적 `Dropped()` 값을 더함. 반복 콜백에서 중복 합산될 수 있음 |
| out-of-order / reorder span | UUIDv7 ID의 역전과 시각 차이. 연속 순번 누락이나 전체 메시지 정합성의 검증 지표는 아님 |
| publish ack `status` | `accepted`, `error` |
| publish failure `subject_kind` | `msg`, `event` |
| 무시한 room event `reason` | `invalid`, `unknown_type` |

send queue overflow는 미전송 프레임 수가 아니며, NATS dropped도 정확한 누락 메시지 수로 해석하지 않는다.

### 메시지 영속화

| 메트릭 | 타입 | 계측 라벨 |
|--------|------|-----------|
| gochat_chat_persistence_lag_seconds | histogram | - |
| gochat_chat_persistence_batch_size | histogram | - |
| gochat_chat_persistence_retry_total | counter | - |
| gochat_chat_persistence_dlq_total | counter | - |
| gochat_chat_persistence_pending | gauge | - |
| gochat_chat_persistence_circuit | gauge | - |
| gochat_chat_persistence_workers | gauge | - |
| gochat_chat_persistence_oldest_pending_age_seconds | gauge | - |

서비스: **chat-service**. JetStream에서 가져온 메시지를 MongoDB에 저장하는 경로를 관측한다.

| 항목 | 의미와 조회 시 주의점 |
|------|----------------------|
| lag | 메시지 `createdAt`부터 저장 성공 처리까지의 시간. `createdAt`은 기본적으로 UUIDv7에서 복원하므로 JetStream 수락 시각과 다를 수 있음 |
| pending | shared consumer의 `NumPending + NumAckPending`. 여러 Pod가 같은 값을 관측하므로 `sum` 대신 `max` 등으로 중복 제거 |
| oldest pending age | 스트림에 남은 가장 오래된 메시지의 시각부터 경과 시간 |
| circuit | Pod별 회로 상태: `0=closed`, `1=open`, `2=half-open` |
| workers | Pod에 설정된 worker 수. 실행 중인 batch 수는 아님 |
| retry | delayed NAK 시도 횟수 |
| DLQ | DLQ 발행 성공 횟수. DLQ에 남은 메시지 수와는 별개 |
| messages saved | 저장 성공 또는 내용이 같은 중복으로 처리한 건수. 고유 문서 수는 아님 |

#### 대시보드 집계 기준

Operations Overview와 Data Persistence의 저장 패널은 다음 기준으로 집계한다. 수집되지 않은 값은 0으로 대체하지 않는다.

| 항목 | 집계 기준 |
|------|-----------|
| 대기량 | namespace별 shared consumer 값을 `max`로 중복 제거한 뒤 합산 |
| 저장 지연 | UUIDv7 생성 시각부터 MongoDB 저장 확인까지의 P99 |
| 재시도·DLQ 발행률 | Pod별 counter의 증가율 합산 |
| 열린 회로 수 | `circuit=1`인 Pod 수 |
| DLQ 잔량 | `nats_stream_total_messages{stream_name="CHAT_PERSIST_DLQ"}`. 앱 Pod·node 필터와 무관하게 선택한 namespace 전체 조회 |

### NATS 서버

NATS exporter가 서버·연결·JetStream 상태를 제공한다. Alloy는 15초마다 수집하며 `namespace`, `service=nats`, `job=nats` 라벨을 붙인다.

메시지 전달 상태는 Realtime Messaging에서, 저장·DLQ 상태는 Data Persistence에서 조회한다.

| 메트릭 | 용도 |
|--------|------|
| nats_varz_in_msgs / nats_varz_out_msgs | 서버 수신·송신 메시지 수 |
| nats_varz_slow_consumers | 서버가 관측한 slow consumer |
| nats_connz_pending_bytes | 연결별 전송 대기 바이트 |
| nats_connz_rtt | 연결 RTT |
| nats_stream_total_messages | stream별 현재 메시지 수, DLQ 잔량 |
| nats_stream_total_bytes / nats_stream_limit_bytes | stream별 byte 사용량과 한도 |

JetStream stream·consumer 상태도 exporter에서 수집한다. 앱의 shared consumer backlog와 Pod별 circuit 상태를 함께 확인한다.

### 데이터베이스

#### PostgreSQL

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
- `operation`은 SQL 첫 유효 line의 첫 키워드를 대문자로 추출한다. sqlc 쿼리의 주요 값은 `SELECT`, `INSERT`, `UPDATE`, `DELETE`다.
- `Exec`, `Query`의 `status`는 호출 반환 오류에 따라 `ok` 또는 `error`다. `QueryRow`는 항상 `status="ok"`를 기록하고 이후 `Scan` 오류를 반영하지 않는다. Duration과 span도 wrapper 호출 반환 시 종료되므로 이후 결과 순회·스캔까지 포함한 전체 작업 시간이나 성공률로 해석하지 않는다.

#### MongoDB

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
- `INSERT_MANY`, `FIND`의 `status`는 호출 반환 오류에 따라 `ok` 또는 `error`다. `FIND_ONE`은 항상 `status="ok"`를 기록하고 이후 `SingleResult.Err()`·`Decode`에서 확인하는 오류를 반영하지 않는다. Duration과 span도 wrapper 호출 반환 시 종료되므로 이후 cursor 순회·디코딩까지 포함한 전체 작업 시간이나 성공률로 해석하지 않는다.

#### Redis

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

- 서비스: api-gateway, websocket-service, user-service
- `state` 값은 `idle`, `used`다.
- `type` 값은 `command`, `pipeline`이다.
- `status` 값은 `ok`, `nil`, `error`이고, `error_type` 값은 `none`, `context_canceled`, `context_timeout`, `other`다.
- 명령별 Redis operation latency는 metric label이 아니라 `redisotel.InstrumentTracing` span에서 확인한다.

### 사용자·인증·채팅 도메인

| 메트릭 | 타입 | 계측 라벨 | 서비스 |
|--------|------|-----------|--------|
| gochat_user_created_total | counter | status | user-service |
| gochat_user_deleted_total | counter | status | user-service |
| gochat_auth_login_total | counter | status | user-service |
| gochat_auth_token_reuse_detected_total | counter | - | user-service |
| gochat_room_join_total | counter | status | user-service |
| gochat_chat_messages_saved_total | counter | - | chat-service |
| gochat_chat_history_fetched_messages | histogram | - | chat-service |
| gochat_hasher_jobs_total | counter | type, status | user-service |
| gochat_hasher_duration_seconds | histogram | type | user-service |
| gochat_hasher_queue_depth | gauge | - | user-service |
| gochat_hasher_queue_full_total | counter | - | user-service |

### 시스템과 Kubernetes

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

### Go 런타임

- 수집 라이브러리: `go.opentelemetry.io/contrib/instrumentation/runtime v0.71.0`
- 런타임 값의 최소 갱신 간격: 15초
- 서비스: 전체 앱 서비스
- `OTEL_GO_X_DEPRECATED_RUNTIME_METRICS`를 켜지 않으므로 deprecated runtime metric은 기본 수집 대상이 아니다.

| 메트릭 | OTel 타입 | 계측 라벨 |
|--------|-----------|-----------|
| go_goroutine_count | observable updowncounter | - |
| go_processor_limit | observable updowncounter | - |
| go_config_gogc_percent | observable updowncounter | - |
| go_memory_used_bytes | observable updowncounter | go_memory_type |
| go_memory_limit_bytes | observable updowncounter | - |
| go_memory_allocated_bytes_total | observable counter | - |
| go_memory_allocations_total | observable counter | - |
| go_memory_gc_goal_bytes | observable updowncounter | - |

`go_memory_type`은 OTel 속성 `go.memory.type`에 대응한다.

`go_memory_limit_bytes`는 Go runtime에 유한한 메모리 제한이 설정된 경우에만 관측된다.

## 4. 트레이스

트레이스는 Tempo에서 요청의 서비스 간 호출과 DB 작업을 추적하는 데 사용한다. 샘플링은 `ParentBased(TraceIDRatioBased(0.1))`로, 루트 스팬은 10% 비율로 선택하고 하위 스팬은 부모의 샘플링 결정을 따른다. 앱 SDK의 배치 전송 timeout은 기본 5초다.

### 자동 계측

| 계층 | 라이브러리/설정 | 서비스 |
|------|----------------|--------|
| HTTP server | `otelhttp.NewMiddleware` | api-gateway, websocket-service |
| gRPC server | `otelgrpc.NewServerHandler` | user-service, chat-service |
| gRPC client | `otelgrpc.NewClientHandler` | api-gateway, websocket-service |
| Redis client | `redisotel.InstrumentTracing` | api-gateway, websocket-service, user-service |

NATS 메시지 경계에서는 트레이스 컨텍스트를 전파하지 않는다. 메시지 전달·저장 경로는 전용 메트릭과 프로파일로 확인한다.

HTTP span name은 `METHOD + " " + telemetry.NormalizePath(path)` 형태다. UUID path segment는 `:id`로 정규화된다.

### DB 수동 스팬

| 스팬 | 어트리뷰트 | 서비스 |
|------|-----------|--------|
| pg.`<SQL_OP>` | db.system=`postgresql`, db.operation=`<SQL_OP>` | user-service |
| mongo.InsertMany | db.system=`mongodb`, db.operation=`InsertMany` | chat-service |
| mongo.Find | db.system=`mongodb`, db.operation=`Find` | chat-service |
| mongo.FindOne | db.system=`mongodb`, db.operation=`FindOne` | chat-service |

PostgreSQL `<SQL_OP>`는 SQL 첫 유효 line의 첫 키워드다. 쿼리의 주요 값은 `SELECT`, `INSERT`, `UPDATE`, `DELETE`다.

### 트레이스에서 생성하는 메트릭

Tempo가 trace 데이터에서 service graph 계열 메트릭을 만들어 Prometheus에 remote write한다.

| 데이터 | 조회 위치 | 내용 |
|--------|-----------|------|
| 서비스 그래프 메트릭 | Prometheus | 서비스 간 관계. `service.name` 차원 포함 |
| TraceQL metrics | Tempo | 트레이스에서 계산하는 메트릭. 과거 데이터 조회 지원 |

## 5. 프로파일

Pyroscope에서 함수별 CPU 사용량, 메모리 사용량, goroutine 상태를 분석한다. 네 Go 서비스 모두 같은 프로파일 종류를 수집한다.

| 프로파일 | 설명 |
|----------|------|
| CPU | CPU 사용 플레임그래프 |
| InuseObjects | 현재 메모리에 살아 있는 객체 수 |
| InuseSpace | 현재 사용 중인 힙 메모리 |
| Goroutines | goroutine 스택 |

- 서비스: api-gateway, websocket-service, user-service, chat-service
- 업로드 주기: pyroscope-go 기본 15s
- Mutex/Block profile은 수집하지 않으며, 관련 Go runtime profiler도 활성화하지 않는다.

## 6. 수집 조건과 필터

### 앱 수집 조건

| 설정 | dev/test/qa 값 | 비어 있을 때 |
|------|----------------|--------------|
| `OTEL_ENDPOINT` | `alloy:4318` | 앱 메트릭·트레이스 OTel 초기화 생략 |
| `PYROSCOPE_ENDPOINT` | `http://pyroscope:4040` | 프로파일 수집 생략 |

앱의 OTel resource에는 환경 변수·프로세스·OS·호스트 정보와 `service.name`, `service.instance.id`가 포함된다.

### 헬스체크와 로그 필터

헬스체크의 수집 여부는 계측 경로에 따라 다르다.

#### 앱 필터

| 대상 | 필터 |
|------|------|
| HTTP 커스텀 메트릭/요청 로그 | `telemetry.MetricsMiddleware`, `middleware.LoggingMiddleware`가 `/health`, `/ready` 스킵 |
| gRPC 서버 커스텀 메트릭/요청 로그 | `telemetry.MetricsServerInterceptor`, `middleware.UnaryLoggingInterceptor`가 `/grpc.health.v1.Health/*` 스킵 |
| gRPC 클라이언트 커스텀 메트릭 | `telemetry.MetricsClientInterceptor`가 `/grpc.health.v1.Health/*` 스킵 |

앱 필터는 `gochat_*` 요청 메트릭과 요청 로그에 적용된다. HTTP/gRPC 자동 계측은 헬스체크도 기록한다. 이 경로에서 생성된 헬스체크 스팬은 Alloy가 제거한다.

#### Alloy 필터

| 시그널 | 필터 |
|--------|------|
| Logs | `/health`, `/ready`, `grpc.health.v1.Health/Check`, `HealthCheck`, `pg_isready`, `adminCommand: ping` 포함 로그 드롭 |
| Traces | `/health`, `/ready`, `grpc.health.*`, `HealthCheck` span 드롭 |
| Metrics | health 전용 필터 없음. HTTP/gRPC 자동 계측 메트릭에도 health request가 포함됨 |

Frontend·Swagger UI·데이터 서비스·부하 테스트 Pod 로그는 Pod 탐색과 라벨 규칙에 따라 수집된다. `postgres-migrate`, `mongo-migrate`와 관측성 서비스(`alloy`, `loki`, `grafana`, `prometheus`, `tempo`, `pyroscope`), 서비스 이름 라벨이 없는 Pod 로그는 제외한다.

## 7. 관련 소스

| 확인할 내용 | 위치 |
|-------------|------|
| 서비스별 계측 연결 | [`cmd/`](../cmd/) |
| OTel·Pyroscope 초기화, HTTP/gRPC·DB 계측 | [`internal/shared/telemetry/`](../internal/shared/telemetry/) |
| 요청 로그·panic 복구 | [`internal/shared/middleware/`](../internal/shared/middleware/) |
| JSON 로그·트레이스 ID 연결 | [`internal/shared/logger/`](../internal/shared/logger/) |
| 도메인 메트릭 정의 | `internal/chat/metrics.go`, `internal/user/metrics.go`, `internal/user/hasher/metrics.go`, `internal/websocket/hub/metrics.go`, `internal/websocket/natsbus/nats.go` |
| 계측 라이브러리 버전 | [`go.mod`](../go.mod) |
| Kubernetes 수집·전달·필터 | [Alloy 설정](../observability/alloy/config.alloy) |
| 백엔드 설정 | [`observability/`](../observability/)의 서비스별 `config.yaml` |
| 대시보드 | [Grafana dashboards](../observability/grafana/provisioning/dashboards/) |
