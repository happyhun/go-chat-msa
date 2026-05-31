# 시스템 설계 문서: Go Chat MSA

## 목차

1. [아키텍처 개요](#1-아키텍처-개요)
2. [시스템 상세 설계](#2-시스템-상세-설계)
3. [주요 의사결정](#3-주요-의사결정)
4. [Kubernetes 배포 설계](#4-kubernetes-배포-설계)
5. [테스트 전략](#5-테스트-전략)
6. [관측성](#6-관측성)
7. [추후 개선사항](#7-추후-개선사항)

---

## 1. 아키텍처 개요

### 1.1 기술 스택

- 언어: Go 1.26+
- 통신:
  - HTTP: `net/http`
  - RPC: `google.golang.org/grpc`, `google.golang.org/protobuf`
  - WebSocket: `gorilla/websocket`
- 데이터:
  - PostgreSQL: `jackc/pgx/v5`
  - MongoDB: `go.mongodb.org/mongo-driver`
  - Redis: `redis/go-redis/v9`
- 인증/보안:
  - 토큰: `golang-jwt/jwt/v5`
  - 암호화: `golang.org/x/crypto`
- 관측성:
  - 계측: `go.opentelemetry.io/otel`
  - 메트릭: `go.opentelemetry.io/otel`
  - 프로파일: `grafana/pyroscope-go`
- 설정/검증:
  - 환경설정: `spf13/viper`
  - 구조체 검증: `go-playground/validator/v10`
- 테스트:
  - 프레임워크: `stretchr/testify`
  - 통합 테스트 컨테이너: `testcontainers/testcontainers-go`
  - E2E: Kubernetes `test` overlay
- 관측성 인프라:
  - 수집: Grafana Alloy (OTel Collector)
  - 로그: Loki
  - 메트릭: Prometheus
  - 트레이스: Tempo
  - 프로파일: Pyroscope
  - 대시보드: Grafana
- 빌드 도구:
  - Protobuf 관리: Buf
  - SQL 코드 생성: sqlc
  - DB 마이그레이션: golang-migrate
  - Mock 생성: mockery

### 1.2 서비스 구성

| 서비스 | 역할 | 프로토콜 | 저장소 |
| :--- | :--- | :--- | :--- |
| api-gateway | REST API 진입점, 인증 위임, 버전 라우팅 | REST | Redis |
| ws-gateway | WebSocket L7 리버스 프록시, Consistent Hashing | HTTP | Redis |
| websocket-service | 실시간 메시지 브로드캐스트, 세션/룸 관리 | WebSocket | - |
| user-service | 사용자 및 채팅방 CRUD, Bcrypt 워커 풀, refresh token 상태 관리 | gRPC | PostgreSQL, Redis |
| chat-service | 메시지 저장 및 조회 | gRPC | MongoDB |

---

## 2. 시스템 상세 설계

### 2.1 API 설계

스키마를 먼저 정의하고 코드를 생성하는 API-first 방식을 따릅니다.

#### 외부 REST

- 가이드: [Zalando RESTful API Guidelines](https://opensource.zalando.com/restful-api-guidelines/)
- 경로: 명사 위주, snake_case 속성명
- 에러: RFC 7807 Problem Details
- 명세: [OpenAPI 스펙](../api/openapi/openapi.yaml)

#### 내부 gRPC

- 가이드: [Google API Design Guide](https://cloud.google.com/apis/design)
- 네이밍: 컬렉션 조회 `List`, 단건 조회 `Get`, 일괄 처리 `Batch`
- 에러: 표준 gRPC 상태 코드
- 명세: [api/proto/](../api/proto/)

#### 요청 검증 책임

| 레이어 | 검증 범위 | 예시 |
| :--- | :--- | :--- |
| 외부 REST | 형식 + 페이지네이션 | 파싱, 필수 필드, limit/offset |
| 내부 gRPC | 비즈니스 규칙 | capacity, username, password |

#### REST API 버전 라우팅

`X-API-Version` 커스텀 헤더로 버전을 지정합니다.

| 헤더 값 | 라우팅 대상 | 비고 |
| :--- | :--- | :--- |
| 없음 | muxV1 | 기본값, 하위 호환성 보장 |
| v1 | muxV1 | 명시적 V1 |
| v2 | muxV2 → muxV1 폴백 | V2 미정의 시 V1으로 라우팅 |

API 버전 관리는 크게 두 가지 방식이 있습니다.

- **URL path 버전** ([Google AIP-185](https://google.aip.dev/185)): 대규모 조직에서 여러 메이저 버전이 공존해야 할 때 유리. 인프라 수준에서 라우팅이 명확하고, 같은 클라이언트가 v1과 v2를 동시에 호출 가능. Google, GitHub, Stripe 등 업계에서 가장 보편적
- **헤더 기반 버전** ([Zalando Rule #113~#115](https://opensource.zalando.com/restful-api-guidelines/#113)): 버전 자체를 만들지 않는 것이 목표. 하위 호환 확장으로 API를 진화시키고, 버전 생성의 허들을 높여 불필요한 breaking change 억제

이 프로젝트는 서비스 수가 적고 API 진화 폭이 크지 않아 Zalando 방식이 적합하다고 판단했습니다. Zalando가 권장하는 미디어 타입 버전(`Accept: application/vnd.example+json;version=2`) 대신 커스텀 헤더를 선택한 이유는 구현 단순성입니다. 미디어 타입 파싱 없이 `VersionRouter`가 헤더 문자열을 비교하여 `muxV1`/`muxV2`로 분기합니다.

### 2.2 인증 전략

#### Access Token

짧은 수명의 HS256 JWT로, `Authorization: Bearer` 헤더로 전달됩니다. 토큰 안에 user_id, username, 만료시간이 포함되어 있어 api-gateway가 매 요청마다 user-service에 위임하지 않고 자체 검증할 수 있습니다.

RS256은 서명자와 검증자가 다를 때(공개키 배포) 의미가 있지만, 이 프로젝트는 api-gateway 한 곳에서만 검증하므로 단일 시크릿을 공유하는 HS256이 적합합니다.

탈취되더라도 만료까지만 유효하므로 피해 범위가 제한되며, 별도 폐기 메커니즘 없이 만료에 의존합니다.

#### Refresh Token

Access Token 재발급에 사용됩니다. JWT와 달리 토큰 자체에 정보를 담을 필요가 없으므로 UUID로 생성한 opaque token을 사용합니다. Opaque token은 토큰만 봐서는 누구의 것인지, 언제 만료되는지 알 수 없고 서버 상태 저장소 조회가 필요합니다.

저장소 유출에 대비하여 토큰 원문은 저장하지 않고 SHA-256 digest만 사용합니다. refresh token은 영구 도메인 데이터가 아니라 만료 시간이 있는 인증 세션 상태이므로 PostgreSQL 테이블이 아니라 Redis TTL key로 관리합니다.

전달은 `HttpOnly`, `SameSite=Strict` 쿠키를 사용하여 XSS/CSRF를 방어합니다. (운영 환경에서는 `Secure` 추가)

현재 프론트엔드는 Nginx 리버스 프록시를 통해 API를 호출하므로 same-origin이며 CORS가 개입하지 않습니다. 프론트엔드가 API를 직접 호출하는 cross-origin 구조로 전환할 경우, `Access-Control-Allow-Origin`에 특정 오리진을 지정하고 `Access-Control-Allow-Credentials`를 설정해야 쿠키 전송이 가능합니다.

토큰 탈취에 대비하여 Refresh Token Rotation을 적용합니다.

1. 로그인 성공 시 `auth:rt:active:{digest}` key를 TTL과 함께 생성합니다.
2. refresh 요청이 들어오면 Lua script가 기존 active token 삭제, used tombstone 생성, 새 active token 생성, 사용자별 active token index 갱신을 원자적으로 수행합니다.
3. 이미 used tombstone으로 남은 토큰이 다시 들어오면 탈취 또는 재사용 공격으로 간주하고 해당 사용자의 active refresh token을 모두 폐기합니다.
4. 로그아웃은 해당 refresh token digest만 삭제합니다. 회원탈퇴는 `auth:rt:user:{userID}` index를 기준으로 사용자의 모든 refresh token을 삭제합니다.
5. Redis TTL이 만료 데이터를 자동 제거하므로 별도 token purge loop나 CronJob은 필요하지 않습니다.

이 선택은 RDB에 인증 세션을 저장하던 초기 설계에서 바뀐 부분입니다. 초기에는 저장소가 PostgreSQL뿐이었기 때문에 refresh token을 `refresh_tokens` 테이블에 넣고 만료 토큰을 주기적으로 삭제했습니다. 그러나 K8s로 넘어가면서 서비스 replica가 늘어나면 background purge loop가 중복 실행되는 문제가 생겼고, CronJob으로 분리하더라도 “TTL 임시 상태를 RDB에 저장한 뒤 다시 정리 job을 운영하는 구조”가 남았습니다.

Redis로 옮기면 TTL, 원자적 Lua script, 빠른 key-value 접근을 활용할 수 있어 인증 세션 상태에는 더 자연스럽습니다. 대신 Redis 장애 시 로그인/refresh/logout 같은 인증 상태 변경이 실패할 수 있으므로 fail-closed로 처리합니다. 즉, 인증 상태를 확인하거나 갱신할 수 없으면 토큰을 허용하지 않습니다. 사용자 계정, 채팅방, 멤버십처럼 복구와 관계 무결성이 중요한 데이터는 계속 PostgreSQL이 source of truth이고, Redis는 만료 가능한 인증 세션과 WebSocket control plane 상태에만 사용합니다.

관련 흐름은 [인증 및 WebSocket 티켓 발급 시퀀스](diagrams/seq-auth-ticket.mmd)와 [Refresh Token rotation 시퀀스](diagrams/seq-refresh-token-rotation.mmd)에서 확인할 수 있습니다.

#### WebSocket 티켓

WebSocket은 `Authorization` 헤더를 지원하지 않아 URL 쿼리 파라미터로 인증 정보를 전달해야 합니다. JWT를 직접 URL에 노출하면 서버 로그, 브라우저 히스토리 등에 토큰이 남는 보안 위험이 있습니다.

이를 피하기 위해 연결 전에 UUID opaque token으로 30초 TTL의 일회성 티켓을 발급합니다. 티켓은 Redis에 `ws:ticket:{uuid}` 키로 저장되며, 사용 즉시 원자적으로 소비됩니다. TTL 만료는 Redis가 자동 처리합니다.

ws-gateway 수평 확장 시에도 모든 인스턴스가 같은 Redis를 공유하므로 티켓 발급/검증의 정합성이 보장됩니다.

#### 내부 통신 시크릿

ws-gateway는 공개 엔드포인트(`/ws/ticket`, `/ws`)와 내부 엔드포인트(`/internal/*`)를 같은 포트에서 서빙합니다. 네트워크 레벨에서 분리되어 있지 않으므로, api-gateway가 ws-gateway의 내부 API를 호출할 때 `X-Internal-Secret` 헤더로 요청의 출처를 검증합니다. 시크릿은 환경설정으로 주입하는 고정 문자열(pre-shared key)입니다.

시크릿 비교 시 타이밍 공격을 방지하기 위해 `crypto/subtle.ConstantTimeCompare`를 사용합니다.

현재 api-gateway와 ws-gateway 두 서비스만 정적 시크릿을 공유합니다. 추후 내부 통신 보안을 더 강화하려면 mTLS 도입 등이 필요합니다.

### 2.3 처리율 제한 전략

무차별 대입, API 오남용, 도배 등을 방어하기 위해 Token Bucket 계열 알고리즘으로 처리율 제한을 적용합니다. 초과 시 429(Too Many Requests)로 거부합니다. 내부 통신 경로(`/internal/*`)는 제한 대상에서 제외합니다. 키 타입은 `string`으로 통일하고, 호출자가 용도에 맞게 키를 포맷합니다. (Client IP, User ID, `userID:roomID` 등)

#### HTTP 미들웨어 (api-gateway, ws-gateway)

| 정책 | 기준 키 | 방어 목적 | RPS / Burst | TTL |
| :--- | :--- | :--- | :--- | :--- |
| 전역 익명 | Client IP | 인증 전 무차별 대입 방어 | 5 / 10 | 10m |
| 인증 사용자 | User ID | API 오남용 방어 | 10 / 20 | 1h |
| 연결 수립 | User ID | 핸드셰이크 단계 세션 점유 방어 | 2 / 5 | 1h |

익명 정책의 클라이언트 IP는 `X-Forwarded-For` 헤더의 첫 번째 값에서 추출합니다. (운영 환경에서는 Trusted Proxy 기반 파싱 필요)

다중 인스턴스 간 카운트 정합성을 보장하기 위해 `redis_rate/v10`(GCRA 알고리즘 + Lua 스크립트)을 사용합니다. Redis 장애 시 미들웨어는 fail-open(요청 통과 + warn log)으로 동작하여 일시 장애가 서비스 다운으로 번지는 것을 방지합니다.

#### WebSocket 세션 (websocket-service)

| 정책 | 기준 키 | 방어 목적 | RPS / Burst | TTL |
| :--- | :--- | :--- | :--- | :--- |
| 세션 내부 | User ID + Room ID | 도배 억제 | 2 / 5 | 1h |

HTTP 미들웨어가 아닌 WebSocket `readPump` 안에서 메시지 단위로 동작합니다. 같은 채팅방의 세션이 Consistent Hashing으로 한 노드에 모이는 어피니티 덕분에 노드 간 동기화가 불필요합니다.

메시지 hot path latency를 우선하여 인메모리 토큰 버킷을 사용합니다. 단일 락 병목을 피하기 위해 `hash/maphash`로 키를 해싱하여 64개 샤드에 분배하고, 샤드별 `sync.Mutex`로 락 경합을 줄입니다. 비활성 버킷은 TTL 기반으로 주기적으로 정리합니다.

### 2.4 데이터 모델 및 인덱스 전략

#### 저장소 선택

사용자/채팅방은 관계가 중요하고(방장, 멤버십, 참조 무결성), 채팅 메시지는 단순 append 위주에 스키마 변경 가능성이 높습니다. 관계형 데이터에는 PostgreSQL, 메시지 저장에는 MongoDB를 사용하여 각 특성에 맞는 저장소를 선택했습니다.

#### 데이터 저장소 버전 기준

dev/test K8s 환경의 데이터 저장소는 특정 최신 major 기능이 필요하지 않으면 최신 major보다 한 단계 낮은 supported major를 기본으로 선택합니다. 충분히 운영 검증되고 레퍼런스가 많은 버전으로 실행 기준선을 안정화하기 위한 선택입니다.

| 저장소 | 현재 이미지 | 적용 기준 |
| :--- | :--- | :--- |
| PostgreSQL | `postgres:17` | 트랜잭션, row lock, `pg_trgm` 중심이라 최신 major 전용 기능보다 운영 레퍼런스를 우선합니다. |
| MongoDB | `mongo:7.0` | 메시지 저장, unique index, TTL index 중심이라 8 계열 전용 기능이 필요하지 않고, 커널/TCMalloc 이슈를 피합니다. |
| Redis | `redis:7-alpine` | TTL, Lua script, keyspace notification 중심이라 최신 major 신규 기능보다 검증된 운영 사례를 우선합니다. |

운영 배포로 확장할 때는 patch tag 또는 image digest pinning과 백업/복구, HA, 업그레이드 리허설을 별도로 설계합니다.

#### PostgreSQL (User Service)

사용자, 채팅방, 멤버십을 관리합니다. 리프레시 토큰은 TTL이 있는 임시 인증 상태이므로 Redis에서 관리합니다.

| 테이블 | 용도 |
| :--- | :--- |
| users | 사용자 계정 |
| rooms | 채팅방 |
| room_members | 채팅방 멤버십 (복합 PK: `user_id`, `room_id`) |

PK는 애플리케이션에서 UUID v7로 생성합니다.

- 이벤트 발생 시점의 타임스탬프가 ID에 포함되어, 네트워크 지연이나 재시도로 DB 도달 순서가 바뀌어도 원래 발생 순서 유지
- INSERT 전에 ID를 알 수 있어 DB 왕복 없이 INSERT 구성 가능
- v4 대비 시간 순서 보장으로 B-tree 순차 삽입, 페이지 분할 감소
- `created_at`은 UUID에서 추출 가능하지만 명시적 조회를 위해 별도 유지

인덱스:

| 대상 | 종류 | 용도 |
| :--- | :--- | :--- |
| `users.username` | UNIQUE | 로그인, 중복 검사 |
| `room_members.(user_id, room_id)` | PK (복합) | 멤버십 조회 |
| `room_members.room_id` | INDEX | 방별 멤버 목록 |
| `rooms.manager_id` | INDEX | 방장별 방 조회 |
| `rooms.name` | GIN (`pg_trgm`) | 방 이름 중간 일치 검색(`ILIKE '%keyword%'`) |

방 이름 중간 일치 검색(`ILIKE '%keyword%'`)에 `pg_trgm` 확장 + GIN 인덱스를 사용합니다.

- B-Tree는 접두사 비교만 가능하여 중간 일치 시 Seq Scan 풀백
- `pg_trgm`은 텍스트를 3-글자 단위(trigram)로 분해하여 GIN에 저장, 중간 일치에도 인덱스 활용 가능
- `gin_trgm_ops`가 `ILIKE`를 지원 연산자로 등록하므로 `LOWER()` 함수형 인덱스 불필요
- 한글은 음절 단위로 trigram 생성되어 정상 동작
- Full-Text Search는 한국어 사전 미내장, Elasticsearch는 방 이름 검색 규모 대비 인프라 비용 과도

트랜잭션 보호:

트랜잭션 실행 함수(`runInTx`)를 외부에서 주입받는 구조입니다. 운영 환경에서는 실제 트랜잭션(begin/commit/rollback)을, 단위 테스트에서는 mock을 주입합니다.

여러 쿼리가 원자적으로 실행되어야 하거나 조회-수정 사이 경쟁 조건이 발생할 수 있는 연산은 트랜잭션 및 `SELECT FOR UPDATE` 행 잠금으로 보호합니다.

| 연산 | 보호 대상 | 잠금 방식 |
| :--- | :--- | :--- |
| 채팅방 참여 | 정원 초과 방지 | `rooms` 행 `FOR UPDATE` |
| 채팅방 생성 | 방 생성 + 방장 멤버 추가 원자성 | 트랜잭션 래핑 |
| 채팅방 수정 | 정원 축소 시 현재 인원 수 검증, 참여/삭제와의 경쟁 조건 방지 | `rooms` 행 `FOR UPDATE` |
| 채팅방 삭제 | 참여와의 경쟁 조건 방지, 방장 권한 검증 원자성 | `rooms` 행 `FOR UPDATE` |
| 채팅방 나가기 | 방장 위임 경쟁 조건 방지, 빈 방 자동 삭제 | `rooms` 행 `FOR UPDATE` |

#### Redis (Auth Session State)

리프레시 토큰은 만료 시간이 있는 bearer credential이므로 Redis TTL key로 관리합니다. 서버에는 토큰 원문을 저장하지 않고 SHA-256 digest만 key에 사용합니다.

| Key | 값 | 용도 |
| :--- | :--- | :--- |
| `auth:rt:active:{digest}` | `userID` | 현재 사용 가능한 refresh token |
| `auth:rt:used:{digest}` | `userID` | 이미 회전된 refresh token tombstone |
| `auth:rt:user:{userID}` | active digest sorted set | 사용자 단위 revoke-all |

토큰 갱신은 Lua script로 active token 삭제, used tombstone 생성, 새 active token 생성, user index 갱신을 원자 처리합니다. 이미 used tombstone이 있는 토큰이 다시 들어오면 재사용 공격으로 판단하고 해당 사용자의 active refresh token을 모두 폐기합니다.

#### MongoDB (Chat Service)

채팅 메시지 저장용(`messages` 컬렉션)으로 사용합니다.

| 인덱스 | 종류 | 용도 |
| :--- | :--- | :--- |
| `{ roomId, clientMsgId }` | UNIQUE | 클라이언트 메시지 중복 방지 |
| `{ roomId, sequenceNumber }` | UNIQUE | 방별 메시지 순서 보장 |
| `{ createdAt }` | TTL (90일) | 오래된 메시지 자동 파기 |

TTL 90일은 채팅 서비스 특성상 오래된 메시지의 조회 빈도가 낮고, 저장 비용을 억제하기 위한 값입니다.

#### 스키마 마이그레이션

스키마와 인덱스는 마이그레이션 파일에서만 관리합니다.
- 개발 환경과 통합 테스트에서 `golang-migrate/migrate` 공통 사용
- 인덱스 생성은 레포지토리 코드에서 제외, 마이그레이션 스크립트에 일임
- 타임스탬프 기반 버전 관리로 파일명 충돌 방지

### 2.5 채팅방 동작

| 동작 | 설명 | 구현 방식 | 시스템 메시지 |
| :--- | :--- | :--- | :--- |
| 참여 | 채팅방 멤버로 등록 | REST API | "OOO님이 들어왔습니다" |
| 나가기 | 채팅방 멤버에서 탈퇴 | REST API | "OOO님이 나갔습니다" |
| 삭제 | 채팅방 삭제 | REST API | 없음 |
| 접속 | 채팅방 화면 진입 | WebSocket 핸드셰이크 | 없음 |
| 접속 해제 | 채팅방 화면 이탈 | WebSocket Close | 없음 |

### 2.6 회원 라이프사이클

현재 프로젝트는 회원탈퇴를 기본 하드 삭제로 처리합니다. 이 프로젝트는 포트폴리오와 학습 목적의 채팅 시스템이며, 법적 보관, 감사 로그, 결제/정산처럼 탈퇴 후에도 사용자 row를 일정 기간 보존해야 하는 요구사항이 없습니다. 그래서 soft delete + grace period + retention batch를 기본값으로 두면 기능보다 운영 복잡도가 먼저 커집니다.

탈퇴 요청은 다음 순서로 처리합니다.

1. 비밀번호를 재검증합니다.
2. 가입한 방마다 LeaveRoom 로직을 적용합니다. 사용자가 방장이면 다른 멤버에게 방장을 위임하고, 혼자 있는 방이면 방을 삭제합니다.
3. Redis에 저장된 해당 사용자의 refresh token을 모두 폐기합니다.
4. `users` row를 삭제합니다. `room_members`는 FK cascade로 정리됩니다.

이 선택의 장점은 스키마와 쿼리가 단순해진다는 점입니다. 모든 사용자 조회에서 `deleted_at IS NULL` 조건을 기억할 필요가 없고, 동일 username도 삭제 직후 바로 재사용할 수 있습니다. 단점은 삭제 후 복구나 grace period 정책을 제공하지 않는다는 점입니다. 이후 보관 요구가 생기면 그때 soft delete 컬럼, retention 정책, 사용자명 freeze 정책, 감사 로그를 함께 설계하는 것이 맞습니다. 이 요구가 없는 현재 단계에서는 하드 삭제가 더 정직한 기본값입니다.

### 2.7 세션 생명주기

#### WebSocket 연결 수립

1. 클라이언트가 WS Gateway에 티켓을 제시하여 WebSocket 연결을 요청합니다.
2. WS Gateway가 티켓을 검증하고, Consistent Hashing으로 대상 노드를 선택하여 프록시합니다.
3. WebSocket Service의 Router가 User Service에 멤버십을 검증한 뒤 연결을 업그레이드합니다.
4. Router가 Manager에 세션 등록을 요청하면, Manager가 해당 채팅방의 Hub를 찾거나 새로 생성합니다.
5. Hub가 세션을 생성하고 읽기/쓰기 펌프를 가동합니다.

#### 세션 충돌 감지

같은 유저가 같은 방에 중복 연결하면 기존 세션을 끊습니다.

1. 새 세션의 `user_id`가 이미 등록되어 있다면 충돌로 간주합니다.
2. 기존 세션에 `type: "conflict"` 메시지를 전송하고 채널을 닫습니다.
3. 큐에 남은 메시지를 비운 뒤 커넥션을 끊고, 새 세션을 등록합니다.

#### 세션 강제 종료

방장이 채팅방을 삭제할 때 해당 방의 모든 세션을 종료합니다.

1. API Gateway를 통해 삭제 API를 호출합니다.
2. User Service가 해당 방 row를 삭제합니다.
3. API Gateway가 비동기로 WS Gateway에 요청을 보내 해당 방의 모든 세션을 강제 종료합니다.
4. 이후 해당 방은 검색이나 조회에서 사라집니다.

### 2.8 메시지 흐름

WebSocket은 실시간 전송 전용이고, 히스토리 조회나 동기화는 REST API로 클라이언트가 직접 합니다.

#### 전송 및 브로드캐스트

1. 클라이언트가 `client_msg_id`를 담아 메시지를 보냅니다.
2. Session의 `readPump`에서 필수 필드(`content`, `client_msg_id`)를 검증하고, rate limit을 체크합니다.
3. Hub의 LRU 캐시에서 `client_msg_id`를 검사해 중복이면 버립니다.
4. 검증된 메시지는 방의 모든 참여자에게 즉시 브로드캐스트합니다. 전송 버퍼가 가득 찬 세션에는 연결을 끊지 않고 해당 메시지를 버립니다. (Load Shedding)

#### 멱등성과 저장

브로드캐스트 완료 후 비동기로 저장합니다.

- 저장 실패 시 재시도 워커가 jitter 적용 지수 백오프로 최대 5회 재시도
- 재시도 시 MongoDB의 `{ roomId, clientMsgId }` 유니크 인덱스가 중복 삽입 방지
- 영속화 채널이 꽉 차면 해당 메시지의 저장을 드랍. 브로드캐스트는 이미 완료된 상태

#### 메시지 동기화

REST API(`GET /rooms/{id}/messages`)에서 `last_seq` 쿼리 파라미터 유무로 두 가지 조회 방식을 분기합니다.

- `last_seq` 없음: 최근 메시지를 `limit`만큼 로드. 처음 입장하거나 오래 비운 뒤 사용
- `last_seq` 있음: 해당 시퀀스 이후의 누락분을 시간순으로 보충. 재연결 시 사용

클라이언트는 로컬에 저장된 `last_seq`를 기준으로 동작합니다.
- 최초 진입 시 API로 최근 메시지를 불러오고 가장 큰 `sequence_number`를 `last_seq`로 저장
- 실시간 수신 중에는 WebSocket 메시지를 받으면서 `last_seq` 갱신
- 재연결 시 `last_seq` 이후의 누락분을 REST API로 보충

#### 메시지 작성자 표시

메시지는 `senderId`만 저장하고 username은 저장하지 않습니다(정규화). 클라이언트가 메시지 페이지를 받으면 unique `sender_id`를 모아 `GET /users?ids=...`로 일괄 조회하여 user_id → username 매핑 캐시를 누적 갱신합니다(Slack/Discord 패턴).

방을 떠난 사용자의 메시지도 user row가 살아있으면 정상적으로 username을 반환받습니다. 회원탈퇴로 user row가 삭제된 사용자는 일괄 조회 응답에서 제외되며, 클라이언트가 `(탈퇴한 사용자)` fallback으로 표시합니다. 메시지에는 `senderId`만 남기기 때문에, 작성자 표시 실패가 메시지 조회 자체를 깨뜨리지 않습니다.

대안으로 메시지에 username을 박제(비정규화)하는 패턴도 있으나, 본 프로젝트는 username 변경 기능이 없어 비정규화 이득이 없고 정규화가 변경 범위가 작습니다.

### 2.9 설정 관리

공통 설정 타입은 `internal/shared/config` 패키지에서 일괄 정의하고, 각 서비스는 이를 가져다 조합합니다.

- **공통 타입 (`shared/config`)**: `HTTPServerConfig`, `JWTConfig`, `RateLimitConfig` 등 여러 서비스가 공유하는 설정 빌딩블록. validate 태그를 포함하여 로딩 시점에 검증합니다.
- **서비스별 Config**: 공통 타입을 임베딩(`mapstructure:",squash"`)하거나 필드로 조합하여 서비스 고유의 설정 트리를 구성합니다. 서비스마다 필드 구성이 다른 래퍼 구조체(예: `RateLimitConfig`, `ServiceRegistry`)는 각 서비스 config 파일에 정의합니다.

#### 환경 격리

K8s-only 실행 경로를 기준으로 설정 파일은 Kustomize가 소유합니다.
- `deploy/k8s/base/apps/config/app/base.yaml`: 환경 독립 기본 설정값
- `deploy/k8s/overlays/dev/apps/config/app/override.yaml`: dev 환경 override 파일
- `deploy/k8s/overlays/test/apps/config/app/override.yaml`: test 환경 override 파일
- `deploy/k8s/overlays/dev`: 로컬 개발 smoke용 K8s 설정
- `deploy/k8s/overlays/test`: E2E correctness 검증용 K8s 설정

K8s base는 `foundation`, `migrations`, `observability`, `apps`로 나누어 phase overlay가 필요한 리소스 묶음만 직접 참조합니다. 앱 런타임 설정은 독립 phase가 아니라 apps phase의 일부입니다.

이미지에는 설정 파일을 포함하지 않습니다. 각 서비스는 K8s가 마운트한 `/app/configs/base.yaml`과 `/app/configs/override.yaml`을 순서대로 로드합니다. 어떤 override가 들어갈지는 Kustomize overlay가 결정합니다.

#### 상수 vs YAML

환경별 차이 여부와 정책적 변경 가능성을 기준으로 분리합니다.

- **Go 상수**: 환경마다 동일하고 변경 가능성이 거의 없는 값. 채널 버퍼 크기, 배치 사이즈, 최대 메시지 크기, 업그레이더 버퍼 등
- **YAML**: 환경마다 달라지거나 정책적으로 변경될 수 있는 값. 타임아웃, 시크릿, 호스트 주소, 처리율 제한 정책 등

#### 환경변수 주입

YAML 키는 환경변수로 오버라이드할 수 있습니다. viper의 `SetEnvPrefix("APP")` + `AutomaticEnv()` 조합으로 `APP_` 접두사가 붙은 env만 자동 적용되며, viper 경로의 `.`은 `_`로 치환됩니다. 예: `WEBSOCKET.ADVERTISED_ADDR` → `APP_WEBSOCKET_ADVERTISED_ADDR`. base.yaml에 `""` + `validate:"required"`로 두면 환경별 매니페스트에서 반드시 주입하도록 강제할 수 있습니다(12-factor app III. Config).

`APP_` 접두사는 호스트 환경의 다른 env가 우리 설정을 silent하게 덮어쓰는 사고를 막기 위한 namespace입니다. 특히 K8s legacy service discovery는 같은 네임스페이스의 모든 Service 이름을 환경변수로 자동 주입(예: `redis` Service → `REDIS_SERVICE_HOST`, `REDIS_PORT_6379_TCP`)하므로, prefix 없이 `AutomaticEnv()`만 호출하면 우리 `REDIS.*` 설정이 K8s 주입 값으로 자동 덮어씌워질 수 있습니다. 같은 이유로 Spring Boot(`SPRING_*`), Rails(`RAILS_*`), AWS SDK(`AWS_*`) 등도 동일한 prefix 패턴을 사용합니다.

### 2.10 우아한 종료

각 서비스는 `errgroup`으로 종료 순서를 관리하며, HTTP/gRPC 서버의 표준 graceful shutdown을 따릅니다. WebSocket Service는 세션과 영속화 파이프라인 때문에 종료 순서가 가장 정교합니다.

1. 종료 신호를 받으면 HTTP 서버가 새 연결 수신을 중단하고 진행 중인 HTTP 요청 완료를 기다립니다.
2. Manager가 모든 Hub에 drain을 지시합니다. Hub는 신규 register/broadcast를 거절하고, 이미 `broadcastCh`에 들어온 메시지를 계속 fan-out합니다.
3. Hub가 fan-out한 메시지는 ack 가능한 persistence task로 Manager의 배치 워커에 전달됩니다. Hub는 `broadcastCh`가 비고 `pendingPersist == 0`이 될 때까지, 또는 `shutdown_timeout`이 만료될 때까지 기다립니다.
4. Manager는 모든 Hub가 멈춘 뒤 `persistCh`를 닫고, 배치 워커와 retry worker가 남은 저장 작업을 처리하도록 기다립니다.
5. gRPC 연결과 텔레메트리 수집기를 정리합니다.

Go `http.Server.Shutdown`은 WebSocket처럼 hijack된 연결을 기다리지 않으므로, WebSocket drain은 HTTP 서버가 아니라 Hub/Manager 레벨에서 별도로 수행합니다. Timeout이 발생하면 종료는 계속 진행하지만 `gochat_ws_persist_drain_total{status="timeout"}`와 duration metric을 남겨 0-loss 보장이 깨진 상황을 관측 가능하게 합니다.

---

## 3. 주요 의사결정

### 3.1 서비스 간 통신

#### gRPC 선택

내부 서비스 간 통신에 REST 대신 gRPC를 선택했습니다.

- `.proto` 파일이 서비스 간 계약 역할을 하여, 필드 추가/삭제 시 컴파일 타임에 양쪽 불일치를 감지
- 서버/클라이언트 스텁이 자동 생성되어 라우팅, 요청 파싱, 응답 직렬화를 직접 작성할 필요가 없음
- Protobuf 바이너리 직렬화로 JSON 대비 페이로드는 2~3배 작고, 파싱은 5~10배 빠름

각 서비스는 타겟당 단일 `grpc.ClientConn`을 공유합니다.  
`ClientConn`은 HTTP/2 multiplexing으로 다중 요청을 동시 처리하며, keepalive 파라미터로 장기 유휴 연결을 유지합니다.  
내부적으로는 DNS resolver가 반환한 backend endpoint마다 subchannel(=long-lived HTTP/2 connection)을 두고, `round_robin` LB 정책으로 RPC를 subchannel들 사이에 분배합니다. 따라서 `ClientConn`은 single connection 추상이 아니라 backend별 connection 집합 + LB 정책을 묶은 채널이며, K8s Headless Service의 multi-A 응답과 결합해 RPC 단위 분산을 달성합니다.  
이 endpoint 집합은 DNS re-resolve가 일어날 때 갱신됩니다. gRPC-Go는 RPC마다 DNS를 조회하지 않으며, 기존 subchannel 연결 실패, channel idle 재진입, `ResolveNow` 트리거 같은 시점에 재조회합니다. 따라서 HPA scale-down은 끊긴 subchannel을 통해 비교적 자연스럽게 감지되지만, scale-up은 기존 subchannel이 모두 정상일 경우 새 Pod가 즉시 round_robin 대상에 들어간다고 보장하지 않습니다. gRPC-Go v1.79.2의 DNS `MinResolutionInterval = 30s`는 주기 polling이 아니라 재조회 rate limit입니다. 운영 HPA에서 scale-up 반영 지연이 문제가 되면 EndpointSlice watch 기반 resolver, xDS/service mesh, connection recycling 같은 보완책을 별도로 검토합니다.  
저장 파이프라인은 배치 워커 풀로, 핸드셰이크 경로는 ws-gateway rate limiter로 동시 호출 수를 억제해 HTTP/2 권장치(연결당 ~100 스트림) 이내로 유도합니다.  
추후 권장치를 초과하는 병목이 관측되면 커넥션 풀을 도입할 예정입니다.

### 3.2 WebSocket 라우팅

WebSocket Service를 다중 인스턴스로 운영할 때, 같은 방의 메시지를 모든 참여자에게 전달하는 방법이 필요합니다. 두 가지 접근이 있습니다.

- **Redis Pub/Sub**: 방 참여자가 여러 노드에 흩어져도 Redis가 중계. 노드 추가/제거가 자유롭지만, 메시지마다 Redis 왕복이 발생하고 외부 인프라 의존 발생
- **Consistent Hashing**: 같은 방의 모든 세션을 한 노드에 모아 인메모리 브로드캐스트. Redis 의존 없이 지연이 낮지만, 노드 변동 시 리밸런싱 비용이 발생

이 프로젝트는 지연 최소화와 구조 단순화를 우선하여 Consistent Hashing을 선택했습니다.

WS Gateway가 `room_id` 기반 Consistent Hashing으로 대상 WebSocket Service 노드를 결정합니다. 일반 해시(`hash(roomID) % nodeCount`)는 노드가 추가되거나 제거될 때 모든 키의 할당이 뒤바뀌어 전체 재연결이 필요하지만, Consistent Hashing은 영향받는 키가 `1/N` 수준으로 최소화됩니다. Consistent Hashing은 결정적이므로 같은 노드 목록이면 어떤 Gateway 인스턴스에서도 동일한 결과가 보장됩니다.

**노드 목록은 동적**입니다. WebSocket Service 인스턴스가 부팅 시 Redis(`wss:member:{addr}` 키, TTL 30s)에 자기 주소를 등록하고 10초마다 `SET key value EX ttl`로 갱신합니다. K8s에서는 이 주소가 `websocket-service` Service DNS나 ClusterIP가 아니라 각 Pod의 `POD_IP:8081`입니다. WS Gateway는 Redis membership에서 고른 owner 주소로 직접 reverse proxy하므로, room owner routing 경로에서는 Service VIP를 거치지 않습니다. Service DNS를 membership에 넣으면 모든 Pod가 같은 `websocket-service:8081` 주소로 보이고, Kubernetes Service가 다시 임의 endpoint로 분산할 수 있어 “room_id -> 특정 Pod owner”라는 Consistent Hashing 결정이 깨집니다. 단순 `EXPIRE`만 호출하면 키가 이미 만료된 경우 false만 반환하고 재등록이 되지 않으므로, heartbeat는 항상 lease refresh로 봅니다. WS Gateway는 keyspace notification + 30초 SCAN 안전망으로 멤버 변경을 watch하고 자기 hash ring을 자동 동기화합니다. K8s 환경에서 HPA 스케일아웃 시 신규 파드가 자연 ring에 포함되고, 사라진 파드는 TTL 만료로 자동 제거됩니다.

애플리케이션은 membership 등록 전에 별도 bootstrap gate를 두지 않습니다. 의존성 준비와 트래픽 투입 여부는 Kubernetes startup/readiness/liveness probe와 Deployment rollout이 제어합니다. `websocket-service`의 최종 `/ready`는 Redis, user/chat gRPC health, persist queue 상태, 자기 `POD_IP:8081`의 hash ring 관측 여부를 확인하므로, K8s는 이 신호를 기준으로 Pod Ready 상태를 관리합니다.

빠른 재시작 시 이전 프로세스의 정리가 새 프로세스의 lease를 지우는 사고를 막기 위해 lease token(프로세스별 UUID v4 — WebSocket ticket·refresh token과 동일한 생성 컨벤션)을 도입했습니다. Redis value에 token을 함께 저장하고, 종료 시 단순 `DEL`이 아닌 compare-and-delete Lua로 자기 token이 맞을 때만 삭제합니다.

흐름과 정합성 패턴은 §3.3 분산 라우팅 정합성에서 다룹니다.

### 3.3 분산 라우팅 정합성

동적 멤버십은 eventual consistency입니다. WS Gateway의 ring과 WebSocket Service의 ring이 멤버십 변경 직후 일시적으로 불일치하여 요청이 잘못된 노드로 라우팅될 수 있습니다. 두 가지 안전망과 클라이언트 재시도로 정합성을 보장합니다.

**per-connection self-check + 503 변환**: WebSocket Service는 Upgrade 직전에 `hashRing.Locate(roomID) != myAddr`이면 **HTTP 421 Misdirected Request**를 응답합니다. WS Gateway는 `proxy.ModifyResponse`로 421을 가로채 자기 Watcher의 `ForceReconcile()`을 호출하고 클라이언트에 **503 Service Unavailable**을 반환합니다. **서버 측 재시도는 없습니다**. 클라이언트의 jittered backoff(1~30초)가 ring이 갱신된 후 재접속을 수행합니다. Cascading retry storm을 피하고 retry 정책을 클라이언트 한 곳에 응축하기 위한 fast fail 설계입니다.

**주기적 owner 재검사 + jitter rebalance**: 각 WebSocket Service의 Manager는 10초 ticker와 멤버십 변경 이벤트로 자기 보유 룸들을 재검사합니다. `ring.Locate(R) != myAddr`인 룸은 `time.AfterFunc(0~10초 jitter)`로 close하여, 스케일아웃 시 다수 룸이 동시에 끊겨 클라이언트 reconnect 트래픽이 폭증하는 thundering herd를 분산합니다.

**rebalance close 전 persist drain**: owner가 바뀐 방은 기존 owner의 Hub가 바로 소켓을 닫지 않습니다. 먼저 신규 ingress를 막고, 이미 fan-out한 메시지의 chat-service 저장 ack를 기다립니다. 이 대기가 없으면 새 owner가 `GetLastSequenceNumber`를 너무 이르게 호출해 같은 sequence number를 다시 발급할 수 있습니다. Drain은 무한 대기하지 않고 `shutdown_timeout` 안에서만 보장하며, timeout은 metric/log로 남깁니다.

**Hash ring 동시성**: HashRing은 라이브러리의 thread-safe `Add/Remove`를 호출하지만, `Set`은 여러 add/remove를 묶은 composite operation이므로 그 자체로는 원자적이지 않습니다. 중간 상태에서 `Locate`가 wrong owner를 반환할 가능성을 막기 위해 wrapper에 `sync.RWMutex`를 두고 `Set`은 Lock, `Locate`는 RLock으로 보호합니다.

**빈 멤버 정책**: Watcher가 SCAN 결과로 멤버 0개를 받으면 Redis 일시 장애일 가능성이 있으므로 기존 ring을 유지합니다(warn 로그). 이때 두 가지 상태가 분리되어 노출됩니다.

- `HashRing.Len()` — 현재 라우팅 가능한 cached ring 크기. 빈 SCAN으로 유지된 경우에도 0이 아닐 수 있음.
- `Watcher.HasObservedMembers()` — 마지막 reconcile에서 실제로 멤버를 1개 이상 보았는지. 빈 SCAN 직후엔 false.

readiness 게이트는 실제 멤버십 저장소를 한 번 이상 관측했는지와 현재 ring이 비어 있지 않은지를 함께 봅니다. 실제 멤버십 저장소에 등록된 인스턴스가 없다면 새 트래픽을 받지 않는 게 맞기 때문입니다. ring을 보수적으로 유지하는 것과 readiness 판단은 분리된 결정입니다.

**Liveness와 readiness 분리**: `/health`는 프로세스 생존 확인용으로 단순 200을 반환합니다. `/ready`는 트래픽 수신 가능 여부를 엄격히 검사합니다.

| 서비스 | `/ready` 검사 |
| :--- | :--- |
| api-gateway | Redis `PING`, user/chat gRPC health |
| ws-gateway | Redis `PING`, membership watcher 관측 여부, hash ring non-empty |
| websocket-service | Redis `PING`, user/chat gRPC health, 자기 주소가 포함된 hash ring, persist queue 사용률 80% 미만 |

user-service와 chat-service의 gRPC health는 각각 PostgreSQL `Ping`, MongoDB `Ping` 결과를 주기적으로 반영합니다. HTTP gateway가 DB에 직접 붙지 않고, 각 backend가 자기 의존성 상태를 gRPC Health Checking Protocol로 노출하는 구조입니다.

게이트웨이만 검증하지 않고 백엔드가 자체 검증하는 양측 확인 패턴은 stateful + 어피니티 기반 분산 시스템의 표준입니다. Kafka(`NOT_LEADER_OR_FOLLOWER`), Cassandra(coordinator forward), Redis Cluster(`MOVED`), MongoDB sharded(`StaleConfigException`), CockroachDB(`NotLeaseHolderError`) 모두 동일한 패턴입니다. 백엔드가 진실의 원천이므로 게이트웨이의 stale 메타데이터를 자체 검증으로 정정합니다.

#### Control Plane vs Data Plane 분리

같은 Redis 인프라를 멤버십 동기화(control plane)에는 keyspace notification + SCAN으로, 메시지 broadcast(data plane)에는 어피니티 + 로컬 fanout으로 사용합니다. 트래픽 특성과 정합성 요구가 다릅니다.

| 측면 | 메시지 broadcast (data plane) | 멤버십 동기화 (control plane) |
|---|---|---|
| 트래픽 | 초당 수천~수만, 지속적 | 분당 1회 미만, 스케일 이벤트 시 |
| 정합성 요구 | at-most-once 손실 = UX 직접 영향 | lag 허용, SCAN reconcile로 자가 치유 |
| 순서 보장 | 룸 내 sequence 단조 증가 필수 | 무관 (snapshot만 정확하면 됨) |
| 선택한 도구 | 어피니티 + hub-local fanout | Redis pub/sub + SCAN 안전망 |

Kafka의 ZooKeeper/KRaft + 자체 메시지 protocol, K8s의 etcd + workload, Istio의 control plane + sidecar와 같은 보편적 분리입니다.

#### Redis keyspace notification 옵션

Watcher는 `__keyspace@<db>__:wss:member:*` 채널을 구독하고 Registry가 SET/DEL을 발행하며 TTL 만료가 expired 이벤트를 만듭니다. Redis는 `K$gx`로 keyspace 이벤트의 String 명령(SET)·generic 명령(DEL)·expired 카테고리만 켜면 충분하므로 dev/e2e/운영 모두 같은 옵션을 사용합니다. 필요 이상 활성화하면 다른 키의 명령 이벤트까지 채널로 흘러 잡음이 됩니다.

#### 다이어그램

[정적 vs 동적 라우팅 비교](diagrams/flow-ws-routing.mmd), [멤버십 동기화 시퀀스](diagrams/seq-membership-sync.mmd), [self-check + 503 변환](diagrams/seq-owner-self-check.mmd), [스케일아웃 시 rebalance](diagrams/seq-rebalance.mmd).

### 3.4 WebSocket 계층 구조

WebSocket Service는 세션 관리, 브로드캐스트, 메시지 저장 등 책임이 다양합니다. 단일 계층에서 처리하면 상태 관리와 동시성 제어가 복잡해지므로, 책임별로 계층을 분리하고 의존 방향을 위에서 아래로 제한했습니다.

```
Router (1)
└── Manager (1)
    ├── Hub (채팅방 A)
    │   ├── Session (유저 1)
    │   └── Session (유저 2)
    └── Hub (채팅방 B)
        └── Session (유저 3)
```

Manager와 Hub는 Actor 모델을 따릅니다. 대부분의 상태 변경은 단일 고루틴의 `select` 루프에서 순차 처리하고, 외부와는 채널 또는 주입된 함수로 통신합니다. 예외적으로 Hub drain 시작과 publish 경합을 막기 위해 작은 accept gate(`RWMutex` + atomic flag)를 두어, drain 이후 새 메시지가 `broadcastCh`에 들어가지 못하게 합니다.

#### 계층별 책임

| 계층 | 책임 |
| :--- | :--- |
| Router | HTTP 요청 수신, `/health`/`/ready`, WebSocket 업그레이드, owner self-check |
| Manager | Hub 생명주기, graceful shutdown, 영속화 워커·retry 워커, 처리율 제한기 |
| Hub | Session 생명주기, sequence 부여, 브로드캐스트, persistence ack 대기 |
| Session | 개별 연결의 송수신, 메시지 검증, rate-limit 검사 |

#### 역참조 차단

자식이 부모를 직접 참조하면 순환 의존이 생깁니다. 상향 통신이 필요한 경우 자식이 부모의 존재를 모르도록 우회합니다.

- **송신 전용 채널**: 부모의 채널 참조만 보유하여 값 전달 (Hub → Manager 영속화)
- **콜백 함수**: 부모가 정의한 함수를 클로저로 감싸 자식에게 주입 (Hub publish 함수·Manager의 처리율 제한기 → Session)
- **인터페이스 추상화**: 구현체가 아닌 인터페이스에 의존 (Router의 메시지 저장소 구현체 → Hub)

### 3.5 Bcrypt 워커 풀

Bcrypt는 brute force 방어를 위해 높은 연산 비용을 요구하는 해시 알고리즘입니다. 제한 없이 고루틴을 생성하면 CPU 경합이 증가하고, 해싱 작업보다 컨텍스트 스위칭에 시간을 소비하면서 개별 요청의 레이턴시가 증가합니다. 워커 풀로 동시 해싱 수를 코어 수로 제한하여 이를 방지합니다.

- 워커 수를 `runtime.GOMAXPROCS(0)`로 고정하여 CPU 바운드 작업의 동시성을 제한
- 대기열이 꽉 차면 즉시 `ErrQueueFull`을 반환해 연쇄 장애 방지

### 3.6 비동기 배치 저장

메시지를 브로드캐스트와 동시에 저장하면, 저장 지연이 브로드캐스트 처리량에 영향을 줍니다. 브로드캐스트와 저장을 분리하여, 실시간 전송은 즉시 처리하고 저장은 배치 워커 풀을 통해 비동기로 처리합니다.

- Hub는 메시지를 fan-out한 뒤 ack 콜백이 붙은 persistence task를 Manager의 `persistCh`에 넣음
- 고정 수의 워커가 채널에서 task를 꺼내 일정 건수 단위로 배치 저장
- 배치가 차지 않아도 타이머가 주기적으로 플러시
- 애플리케이션에서 순서를 부여하므로 워커 간 DB 저장 순서는 무관
- 저장 성공 또는 idempotent success(`AlreadyExists`) 때 task별 ack를 호출하여 Hub의 `pendingPersist`를 감소
- retryable 오류는 Manager retry queue로 이동하며, retry 성공·최종 실패·shutdown drain 전까지 Hub ack를 지연
- 서버 종료나 rebalance close 시 Hub는 이미 fan-out한 메시지의 저장 ack를 기다린 뒤 세션을 닫음

이 구조는 hot path의 브로드캐스트 지연을 낮추는 대신, 저장 실패가 길어질 경우 Hub drain이 `shutdown_timeout`까지 지연될 수 있습니다. 무한 대기는 하지 않으며 timeout 시 metric/log를 남기고 종료를 계속합니다.

### 3.7 PK로 UUID v7 앱 생성

PK는 애플리케이션에서 UUID v7로 생성합니다. v4 대신 v7을 선택하고, DB 생성 대신 앱 생성을 선택한 이유는 다음과 같습니다.

- v4는 B-tree 전체에 랜덤 삽입되어 디스크로 내려간 페이지를 다시 읽는 캐시 미스가 빈번하지만, v7은 시간 순 단조 증가로 끝에 순차 삽입되어 작업 페이지가 메모리에 유지되므로 캐시 히트율이 높음
- auto-increment와 유사한 삽입 패턴이면서도 외부 노출 시 전체 레코드 수나 생성 속도를 추측할 수 없어 열거 공격에 안전
- INSERT 전에 ID가 확정되어 DB 왕복 없이 관련 엔티티의 FK를 즉시 설정 가능
- 비동기 배치 저장 시에도 ID가 이미 존재하므로 브로드캐스트와 영속화를 독립적으로 진행 가능
- 타임스탬프가 생성 시점 기준이므로, 네트워크 지연이나 재시도로 DB 도달 순서가 바뀌어도 원래 발생 순서가 ID에 보존

---

## 4. Kubernetes 배포 설계

이 프로젝트의 K8s 전환 목표는 단순히 `docker compose up`을 `kubectl apply`로 바꾸는 것이 아닙니다. Docker Compose는 한 머신에서 여러 컨테이너를 동시에 띄우는 개발 도구에 가깝고, Kubernetes는 여러 replica가 동적으로 생성·종료되는 환경에서 트래픽 라우팅, readiness, rollout, batch job, 관측성을 함께 다루는 플랫폼입니다. 따라서 앱 코드는 “프로세스 하나가 오래 떠 있다”는 가정에서 벗어나, 여러 파드가 동시에 떠도 중복 실행되면 안 되는 책임과 수평 확장되어야 하는 serving 책임을 분리해야 합니다.

K8s 전환에서 가장 먼저 나눈 기준은 다음입니다.

| 책임 | K8s 리소스 | 이유 |
| :--- | :--- | :--- |
| 계속 요청을 받아야 하는 서비스 | Deployment | replica 수를 조절하고 rolling update 대상이 됨 |
| 내부 라우팅 대상 | Service | 파드 IP가 바뀌어도 안정적인 DNS 이름 제공 |
| gRPC backend discovery | Headless Service | 클라이언트가 backend Pod IP 목록을 직접 보고 round_robin 수행 |
| 일회성 마이그레이션 | Job | 성공/실패가 명확하고 완료 후 종료됨 |
| 주기적 배치 | CronJob | Deployment replica 수와 실행 횟수를 분리 |
| 외부 HTTP/WebSocket 진입점 | Ingress | `/`, `/api`, `/ws-api`, `/ws` path 라우팅 |

MSA 앱 책임 구조는 [MSA 앱 아키텍처](diagrams/flow-msa.mmd), K8s 런타임 배치 구조는 [K8s 런타임 배포 구조](diagrams/flow-k8s-runtime.mmd), overlay 구성은 [K8s overlay 구조](diagrams/flow-k8s-overlays.mmd), bootstrap 순서는 [K8s bootstrap 흐름](diagrams/flow-k8s-bootstrap.mmd)에서 확인할 수 있습니다.

### 4.1 Manifest 구조

K8s manifest는 `base`와 `overlay`로 나눕니다.

```text
deploy/k8s
├── base
│   ├── apps
│   │   └── config
│   ├── foundation
│   ├── migrations
│   └── observability
└── overlays
    ├── dev
    │   ├── foundation
    │   ├── observability
    │   ├── migrations
    │   └── apps
    └── test
        ├── foundation
        ├── observability
        ├── migrations
        └── apps
```

`base`는 공통 리소스의 기본 형태를 정의합니다. 서비스 이름, 포트, probe, label, volume mount, Ingress path 같은 “환경이 바뀌어도 거의 변하지 않는 구조”가 여기에 들어갑니다.

`overlay`는 실행 목적에 따라 달라지는 값을 덮어씁니다.

| Overlay | 목적 | 특징 |
| :--- | :--- | :--- |
| `dev` | 로컬 개발 smoke | 단일 인스턴스 중심, 프론트 접속과 수동 확인 |
| `test` | 자동화된 K8s e2e correctness | 주요 앱 서비스 `replicas: 2`, 멀티 인스턴스 정합성 검증 |
| `qa` | 후속 계획 | k3s 멀티 노드, HPA, k6 부하, rollout/drain 검증 |

이름을 `local`이 아니라 `dev/test/qa`로 정리한 이유는 실행 목적을 분리하기 위해서입니다. `local`은 “내 컴퓨터에서 돈다”는 위치만 말하지만, `dev`는 사람이 기능을 확인하는 환경, `test`는 자동화된 correctness gate, `qa`는 실제 멀티 노드와 부하를 걸 검증 환경이라는 역할을 표현합니다.

### 4.2 Phase Overlay와 Bootstrap 순서

K8s는 manifest를 한 번에 적용할 수 있지만, 이 시스템은 순서가 중요합니다. 앱이 뜨기 전에 DB가 준비되어야 하고, DB schema migration이 끝나기 전에 user-service/chat-service가 트래픽을 받으면 실패합니다. 또한 observability backend가 없는 상태에서 앱을 먼저 띄우면 exporter 실패 로그가 반복되어 실제 문제와 잡음을 구분하기 어렵습니다.

그래서 환경 루트 overlay는 전체 렌더링 확인용 inventory로만 두고, 실제 실행은 bootstrap script가 사용할 phase overlay를 따릅니다.

| Phase | 포함 리소스 | 목적 |
| :--- | :--- | :--- |
| `foundation` | Secret, Postgres, Mongo, Redis | 앱이 의존하는 기본 실행 기반 |
| `observability` | Alloy, Prometheus, Grafana, Loki, Tempo, Pyroscope, Grafana Ingress | 앱 rollout 전에 telemetry 수신 대상 준비 |
| `migrations` | `postgres-migrate`, `mongo-migrate` Job | schema/index를 앱 시작 전에 확정 |
| `apps` | `gochat-app-config` ConfigMap, 앱 Deployment, Service, app Ingress | 실제 serving workload 기동 |

dev/test는 같은 kind 클러스터에 동시에 올라갈 수 있으므로, observability overlay는 Alloy cAdvisor용 `ClusterRole`/`ClusterRoleBinding` 이름을 환경별로 분리합니다. namespaced 리소스는 namespace로 격리하고, cluster-scoped 리소스는 이름으로 격리합니다.

`deploy/k8s/scripts/bootstrap.sh`는 이 순서를 강제합니다.

```text
1. namespace 보장
2. foundation apply
3. Postgres/Mongo/Redis rollout wait
4. observability ConfigMap 생성
5. observability apply
6. Alloy/Prometheus/Grafana/Loki/Tempo/Pyroscope rollout wait
7. migration ConfigMap 생성
8. migration Job 삭제 후 재생성
9. migration Job completion wait
10. OpenAPI ConfigMap 생성
11. apps apply
12. backend/edge Deployment restart와 rollout wait
```

마이그레이션 Job은 재실행 가능하도록 기존 Job을 먼저 삭제한 뒤 다시 만듭니다. Kubernetes Job은 완료된 뒤 같은 이름으로 다시 실행되지 않으므로, dev/test 환경에서 반복 bootstrap을 하려면 이 삭제-재생성 흐름이 필요합니다. migration 실패 시 app rollout을 중단합니다. 스키마가 불확실한 상태에서 앱을 띄워 더 큰 에러를 만들지 않기 위한 fail-fast 전략입니다.

### 4.3 Runtime 리소스 모델

장기 실행 서비스는 Deployment로 둡니다.

| Deployment | 역할 | Service |
| :--- | :--- | :--- |
| `api-gateway` | REST API 진입점 | ClusterIP |
| `ws-gateway` | WebSocket ticket/API 및 WebSocket reverse proxy | ClusterIP |
| `websocket-service` | 실제 WebSocket 세션과 방별 Hub 관리 | 없음 |
| `user-service` | 사용자/방/멤버십 gRPC backend | Headless |
| `chat-service` | 메시지 저장/조회 gRPC backend | Headless |
| `frontend` | 정적 프론트엔드 | ClusterIP |

`api-gateway`, `ws-gateway`, `frontend`는 ClusterIP Service를 사용합니다. Ingress 또는 다른 서비스가 안정적인 DNS 이름으로 접근하면 충분하기 때문입니다. `websocket-service`는 의도적으로 Service를 두지 않습니다. 실제 WebSocket room owner routing은 Redis membership에 등록된 Pod IP로 직접 들어가며, Service VIP를 거치면 Consistent Hashing이 고른 owner가 Kubernetes Service load balancing으로 다시 바뀔 수 있습니다. readiness와 rollout은 Pod/Deployment 상태만으로 판단할 수 있으므로 이 경로에는 Service가 필요하지 않습니다.

반면 `user-service`와 `chat-service`는 Headless Service를 사용합니다. 일반 ClusterIP는 Service 가상 IP 하나로 트래픽을 숨기기 때문에 gRPC 클라이언트 입장에서는 backend Pod 목록을 직접 보기 어렵습니다. 이 프로젝트는 gRPC `dns:///user-service:50051`와 `round_robin` 정책을 사용하므로, DNS가 여러 Pod IP를 반환해야 RPC 단위 분산이 가능합니다. Headless Service는 `clusterIP: None`으로 Service VIP를 만들지 않고 endpoint Pod IP들을 DNS 응답으로 노출합니다.

이 선택의 트레이드오프는 책임 위치입니다. Kubernetes Service가 L4 부하분산을 대신 해주는 구조보다 클라이언트 설정이 더 중요해집니다. 대신 gRPC의 장기 HTTP/2 연결 특성을 고려해 backend별 subchannel을 만들고 RPC를 분배할 수 있어, 다중 user/chat replica에서 더 의도한 방식으로 분산됩니다.

다만 Headless DNS는 endpoint 변경을 gRPC client에 push하는 discovery 채널이 아닙니다. Pod 삭제는 기존 연결 실패로 재조회가 유도되기 쉽지만, Pod 추가는 기존 연결이 건강하면 다음 re-resolve 전까지 반영되지 않을 수 있습니다. 현재 dev/test baseline은 replica 수를 고정해 멀티 인스턴스 정합성을 검증하는 환경이므로 이 한계를 허용합니다. HPA처럼 Pod 수가 동적으로 바뀌는 환경에서는 EndpointSlice/CoreDNS 상태와 실제 Pod별 gRPC request 분포를 함께 보고 scale-up 반영 지연을 측정합니다.

dev/test baseline에서는 CPU request와 CPU limit을 모두 두지 않습니다. 이 두 환경의 목적은 성능 튜닝이 아니라 각각 “로컬에서 앱을 쉽게 띄워 확인하는 것”과 “고정 replica 상태에서 기능 정합성을 검증하는 것”입니다. 여기서 CPU를 예약하면 작은 로컬 머신에서 스케줄링 장벽이 올라가고, CPU limit을 두면 throttling 때문에 테스트 지연이 실제 애플리케이션 병목처럼 보일 수 있습니다. 그래서 dev/test는 CPU를 클러스터가 공유하도록 두고, 성능 기준값은 만들지 않습니다.

memory는 CPU와 다릅니다. 메모리는 압축 가능한 자원이 아니어서 한 Pod가 계속 늘어나면 노드 전체가 불안정해질 수 있습니다. 그래서 dev/test baseline에서도 memory request와 memory limit은 둡니다. request는 로컬 클러스터가 대략적인 메모리 예약량을 계산하게 하고, limit은 runaway allocation이나 observability stack의 예기치 않은 증가를 막는 안전장치입니다. CPU request, HPA target, 부하 중 throttling 여부 같은 운영성 값은 향후 `qa` overlay에서 멀티 노드/k6 관측 결과를 기준으로 별도 설정합니다.

### 4.4 Local Data Layer

Postgres, Mongo, Redis도 dev/test baseline에서는 K8s 안에 띄웁니다. 다만 이들은 운영용 DB manifest가 아니라 K8s 실행 경로와 e2e를 독립적으로 검증하기 위한 local data layer입니다.

| 리소스 | 구현 | 저장소 | 이유 |
| :--- | :--- | :--- | :--- |
| Postgres | Deployment + ClusterIP | `emptyDir` | user/room schema와 migration 검증 |
| Mongo | Deployment + ClusterIP | `emptyDir` | message schema/index와 catch-up 검증 |
| Redis | Deployment + ClusterIP | 메모리 | refresh token TTL, WebSocket ticket, membership 검증 |

운영 환경이라면 StatefulSet, PVC, backup/restore, replication, credential rotation, network policy까지 고려해야 합니다. 하지만 현재 dev/test 목표는 “로컬 K8s에서 앱 실행 모델을 검증하는 것”입니다. 그래서 data layer는 단순하게 두고, 운영형 persistence 설계는 별도 과제로 분리했습니다.

이 결정은 의도적인 절충입니다. 처음부터 운영형 DB 배포까지 넣으면 학습 범위가 지나치게 넓어지고, WebSocket owner, gRPC discovery, readiness, migration, e2e 같은 핵심 마이그레이션 검증이 흐려집니다. 대신 local data layer는 매번 깨끗하게 재생성 가능하고 e2e cleanup도 단순해집니다.

### 4.5 Ingress와 외부 경로

Ingress는 브라우저와 테스트 클라이언트가 접근하는 외부 경로를 하나로 모읍니다.

| Path | Backend | 목적 |
| :--- | :--- | :--- |
| `/` | `frontend` | 프론트엔드 |
| `/api` | `api-gateway` | REST API |
| `/ws-api` | `ws-gateway` | WebSocket ticket 발급 등 HTTP API |
| `/ws` | `ws-gateway` | WebSocket upgrade |
| `/docs` | `swagger-ui` | OpenAPI 문서 UI |
| `/grafana` | `grafana` | 로컬 관측 대시보드 |

같은 kind 클러스터에 `dev`와 `test` namespace를 동시에 띄울 수 있으므로, Ingress는 overlay에서 host를 분리합니다. `dev`는 `dev.gochat.localhost:30080`, `test`는 `test.gochat.localhost:30080`을 사용합니다. namespace만 다르고 host/path가 같으면 ingress-nginx 입장에서는 외부 라우팅 규칙이 충돌할 수 있기 때문에, 환경 경계는 namespace와 Ingress host를 함께 사용해 나눕니다.

kind 로컬 클러스터에서는 control-plane node에만 `30080 -> 80`, `30443 -> 443` host port mapping을 둡니다. 따라서 ingress-nginx controller도 그 node에 떠야 로컬 브라우저 요청이 자연스럽게 Ingress controller로 들어갑니다. `deploy/k8s/clusters/kind-local.yaml`은 control-plane node에 `ingress-ready=true` 커스텀 라벨을 붙이고, `make kind-up`은 ingress-nginx controller Deployment에 `nodeSelector`를 patch해 이 라벨이 있는 node로 스케줄되게 합니다. 이 라벨명은 Kubernetes 표준 라벨이 아니라 kind 로컬 Ingress 진입점을 맞추기 위한 프로젝트 컨벤션입니다.

프론트엔드와 API를 같은 origin 아래에 두는 이유는 인증 쿠키 때문입니다. refresh token은 `HttpOnly` 쿠키로 전달되므로 cross-origin 구조로 만들면 CORS와 credential 정책을 추가로 설계해야 합니다. local K8s baseline에서는 Ingress path routing으로 same-origin을 유지해 인증 흐름을 단순하게 만듭니다.

WebSocket 경로는 일반 HTTP와 달리 upgrade 연결이 길게 유지됩니다. 그래서 Ingress에는 WebSocket timeout 관련 nginx annotation을 명시합니다. 짧은 기본 timeout에 의해 정상 채팅 연결이 끊기는 문제를 피하기 위한 설정입니다.

### 4.6 Probe와 Lifecycle

K8s에서 probe는 단순 헬스체크가 아니라 rollout과 트래픽 라우팅의 기준입니다. 특히 readiness와 liveness를 섞으면 장애 대응이 오히려 나빠질 수 있습니다.

| Probe | 의미 | 이 프로젝트의 기준 |
| :--- | :--- | :--- |
| startupProbe | 느린 초기화를 기다림 | 앱 부팅 중 liveness 오판 방지 |
| livenessProbe | 프로세스가 살아있는지 확인 | HTTP는 `/health`, gRPC는 TCP socket |
| readinessProbe | 트래픽을 받아도 되는지 확인 | HTTP는 `/ready`, gRPC는 native gRPC health |

user-service와 chat-service의 gRPC health는 DB 상태를 반영합니다. 이 값을 liveness에 넣으면 DB가 잠깐 느려졌다는 이유로 애플리케이션 Pod를 재시작하는 악순환이 생길 수 있습니다. 그래서 gRPC 서비스 liveness는 TCP socket으로 두고, readiness에서 DB 의존성을 확인합니다. 준비되지 않은 Pod는 Service endpoint에서 빠져 새 트래픽을 받지 않지만, 프로세스 자체는 재시작하지 않습니다.

WebSocket Service는 더 조심해야 합니다. 연결 중인 세션과 영속화 중인 메시지가 있기 때문에 Pod가 종료될 때 바로 끊으면 in-flight 메시지가 손실될 수 있습니다. 앱 내부의 graceful drain이 이미 fan-out한 메시지의 저장 ack를 기다리도록 설계되어 있으므로, K8s Deployment에는 `terminationGracePeriodSeconds`를 주어 이 drain이 끝날 시간을 확보합니다.

### 4.7 Job/CronJob 판단 기준

K8s에서는 serving process와 batch process를 분리하는 것이 기본 원칙입니다. Deployment replica가 2개면 그 안의 background goroutine도 2번 실행되기 때문에, 주기적으로 한 번만 실행되어야 하는 작업은 Deployment 내부 루프가 아니라 Job/CronJob으로 빼야 합니다.

다만 이번 설계에서는 “CronJob을 어떻게 만들까”보다 “정말 CronJob이 필요한가”를 먼저 다시 봤습니다.

| 후보 작업 | 최종 판단 | 이유 |
| :--- | :--- | :--- |
| refresh token 만료 정리 | 제거 | refresh token source of truth를 Redis TTL로 옮겨 만료 cleanup을 Redis가 담당 |
| 탈퇴 사용자/삭제 방 retention purge | 제거 | 기본 정책을 hard delete로 단순화하여 별도 보관 기간과 purge 대상이 사라짐 |

현재 dev/test K8s overlay에는 애플리케이션 CronJob이 없습니다. 이후 감사 로그, 법적 보관, 휴면 데이터 삭제처럼 실제 배치 요구가 생기면 그때 one-shot command, CronJob manifest, concurrency policy, retry/backoff, 관측 지표를 함께 설계합니다. 이 결정은 “K8s 기능을 보여주기 위해 리소스를 남기는 것”보다 “현재 요구에 맞는 단순한 운영 모델을 유지하는 것”을 우선한 결과입니다.

### 4.8 E2E 실행 기준

Docker Compose 실행 경로는 mainline에서 제거했습니다. 현재 e2e는 K8s `test` overlay가 이미 bootstrap되어 있다는 전제로 실행됩니다.

```bash
make test-up
go test -count=1 -tags=e2e ./test/e2e
```

통합 테스트까지 함께 확인할 때는 같은 `go test` 명령에 build tag를 같이 넘깁니다.

```bash
go test -count=1 -tags=integration,e2e ./...
```

기본 endpoint는 다음입니다. 이 값은 사람이 브라우저로 접속하라고 노출하는 URL이 아니라, 클러스터 밖에서 실행되는 Go e2e runner가 Ingress를 통해 실제 HTTP/WebSocket 경로를 검증하기 위한 테스트 대상입니다.

| 환경변수 | 기본값 | 용도 |
| :--- | :--- | :--- |
| `E2E_GATEWAY_BASE_URL` | `http://test.gochat.localhost:30080/api` | REST API |
| `E2E_WS_BASE_URL` | `http://test.gochat.localhost:30080/ws-api` | WebSocket ticket/API |
| `E2E_K8S_NAMESPACE` | `go-chat-test` | readiness/replica/membership 검증 대상 |

`test` overlay는 api-gateway, ws-gateway, websocket-service, user-service, chat-service를 `replicas: 2`로 고정합니다. HPA를 바로 붙이지 않은 이유는 변수가 너무 많아지기 때문입니다. 먼저 “같은 코드가 두 개 떠도 정합성이 깨지지 않는가”를 고정 replica로 확인해야 합니다. 그 다음에야 HPA, node drain, k6 부하처럼 동적인 조건을 얹을 수 있습니다.

E2E cleanup에서는 Redis `FLUSHALL`을 사용하지 않습니다. Redis에는 테스트 데이터뿐 아니라 WebSocket Service membership과 hash ring 검증 대상도 들어 있습니다. 전체 flush를 해버리면 테스트가 검증해야 할 control plane 상태를 스스로 지우는 셈입니다. 대신 Postgres/Mongo 데이터는 테스트 전용 데이터 정리로 격리하고, Redis는 `auth:rt:*`, `ws:ticket:*`, `rate:*`처럼 테스트 요청이 만든 상태성 key만 선택적으로 삭제합니다. `wss:member:*` membership key는 검증 대상이므로 보존합니다.

### 4.9 Docker Compose 제거와 기준점 보존

Docker Compose는 더 이상 mainline의 실행 경로가 아닙니다. 개발 확인과 e2e 모두 K8s 기준으로 정리했습니다. 다만 Docker 자체는 계속 사용합니다. 로컬 이미지를 빌드하고 kind/OrbStack/K8s 클러스터에 이미지를 제공하기 위해 Dockerfile은 여전히 필요합니다.

Compose 제거 전에 `legacy-compose-baseline` annotated tag를 남겼습니다. 나중에 “Compose 대비 K8s 성능 지표”를 비교하고 싶을 때, mainline에 Compose 파일을 계속 들고 갈 필요 없이 해당 tag를 기준점으로 참조할 수 있습니다.

이 결정의 장점은 현재 실행 경로가 단순해진다는 점입니다. 문서와 e2e가 K8s 하나만 바라보므로 “Compose에서는 되는데 K8s에서는 안 되는” 이중 현실이 줄어듭니다. 단점은 K8s bootstrap이 선행되어야 하므로 처음 실행 장벽이 올라간다는 것입니다. 그래서 README는 K8s dev/test 실행 명령을 authoritative runbook으로 두고, 디자인 문서는 왜 그런 구조가 되었는지를 설명하는 역할로 나눕니다.

---

## 5. 테스트 전략

### 5.1 테스트 구성

| 구분 | 빌드 태그 | 파일 네이밍 | 함수 네이밍 | 실행 방식 |
| :--- | :--- | :--- | :--- | :--- |
| 단위 테스트 | 없음 | `*_test.go` | `TestStruct_Method`, `TestFunc`, `Test_func` | mock/stub/fake/miniredis, 테이블 기반, `t.Parallel()` |
| 통합 테스트 | `//go:build integration` | `*_integration_test.go` | `(s *Suite) TestMethod_Scenario` | testify/suite, 순차 실행 |
| E2E 테스트 | `//go:build e2e` | `test/e2e/*_test.go` | `(s *E2ESuite) TestScenario_##_Name` | testify/suite, 순차 실행 |

### 5.2 작성 원칙

#### 단위 테스트

- 외부 프로세스나 네트워크에 의존하지 않는다. 필요한 의존성은 mock, stub, fake, in-memory test double로 대체한다.
- `miniredis`는 Redis fake로 보고 단위 테스트에서 허용한다. 단, 실제 Redis 서버 설정이나 이벤트 호환성을 검증하는 목적이면 통합 테스트로 승격한다.
- 테이블 기반 서브테스트(`t.Run`)로 정의하여 `t.Parallel()` 병렬 실행
- 서브테스트 네이밍: `Success: 설명` / `Failure: 설명 (에러코드)`

#### 통합 테스트

- Testcontainers로 실제 DB/Redis를 띄우고 시나리오 위주로 검증
- Redis keyspace notification, expire 이벤트, 실제 서버 설정처럼 fake 구현과 운영 Redis의 차이가 의미 있는 동작은 통합 테스트에서 검증한다.
- 데이터 오염 방지를 위해 순차 실행, 매 테스트마다 데이터 초기화

#### E2E 테스트

- K8s `test` overlay로 전체 시스템을 띄우고 블랙박스 검증
- e2e suite는 이미 bootstrap된 `go-chat-test` namespace의 readiness를 확인한 뒤 실행
- 테스트 간 cleanup은 K8s 내부 Postgres truncate와 MongoDB drop으로 수행하며, Redis membership/ring 상태는 검증 대상이므로 전체 flush하지 않음
- 시나리오 번호 순서대로 사용자 여정(user journey)을 이어가며 검증

---

## 6. 관측성

운영 중인 시스템의 내부 상태를 외부에서 파악하려면 관측성이 필요합니다. 특히 MSA에서는 장애 지점이 여러 서비스에 걸칠 수 있어 더 중요합니다. 로그, 메트릭, 트레이스, 프로파일 4가지 신호를 조합하여 이상 감지(메트릭) → 구간 특정(트레이스) → 상세 컨텍스트(로그) → 코드 레벨 병목(프로파일)을 연결합니다.

계측은 벤더 중립적이고 Go 생태계에서 사실상 표준인 OpenTelemetry SDK를 사용합니다. 백엔드를 교체해도 애플리케이션 코드 변경이 필요 없습니다. 백엔드는 Grafana 스택(Loki, Prometheus, Tempo, Pyroscope)으로 통일하여 4가지 신호를 하나의 대시보드에서 통합 조회합니다.

로그, 메트릭, 트레이스는 Grafana Alloy(OTel Collector)가 수집하여 각 백엔드로 라우팅합니다. 프로파일은 `pyroscope-go`가 Pyroscope 서버에 직접 push합니다.

| 신호 | 백엔드 | 용도 |
| :--- | :--- | :--- |
| 로그 | Loki | 이벤트 기록 검색 |
| 메트릭 | Prometheus | 이상 감지 |
| 트레이스 | Tempo | 서비스 간 요청 흐름 추적 |
| 프로파일 | Pyroscope | 코드 레벨 병목 분석 |

### 6.1 로그

`slog`로 JSON 구조화 로깅을 하며, 모든 로그에 `trace_id`와 `span_id`를 자동 주입하여 트레이스와 연결합니다.

- 에러 로그는 발생 지점이 아닌 호출 스택 최상단에서 한 번만 기록
- HTTP 로그에서 민감한 쿼리 파라미터(token, password, secret 등)를 자동 마스킹
- `/health`, `/ready` 로그는 애플리케이션 미들웨어와 Alloy 수집 단계에서 필터링

### 6.2 메트릭

OTel Metrics SDK로 계측하고 OTLP로 Alloy에 push합니다. Alloy가 Prometheus에 remote write합니다. 커스텀 미들웨어/인터셉터/래퍼로 계측하되, Redis는 라이브러리(`redisotel`) 자동 계측을 사용합니다.

PostgreSQL/MongoDB는 메트릭 컨벤션 통일(`gochat_*` prefix, `operation`/`status` 라벨)과 트레이스의 민감 파라미터(`db.statement`에 들어가는 원본 SQL/aggregation pipeline) 노출 통제를 위해 직접 래핑합니다. Redis는 명령(GET/SET 등)이 짧고 표준 enum이라 라이브러리 자동 attribute가 그대로 노출돼도 위험이 작아 `redisotel`을 그대로 채택했습니다.

- HTTP: 요청 수, 지연 시간, 상태 코드
- gRPC: 서버/클라이언트 양쪽 요청 수, 지연 시간, 상태 코드
- PostgreSQL: 쿼리 수, 지연 시간, Pool stats (커스텀 래퍼)
- MongoDB: 쿼리 수, 지연 시간, Pool stats (커스텀 래퍼)
- Redis: Pool stats (`redisotel.InstrumentMetrics` 자동). 명령별 latency는 트레이스로 확인
- WebSocket: 세션, 메시지, 저장 파이프라인 지표
- User: 인증, Bcrypt 워커 풀 지표
- Chat: 메시지 저장, 조회 지표

고카디널리티 방지를 위해 URL 경로의 UUID를 `:id`로 정규화하고, 헬스체크/메트릭 엔드포인트는 수집에서 제외합니다. OTel resource에는 공통으로 `service.name`과 `service.instance.id`가 들어가며, `service.instance.id`는 K8s `POD_NAME`을 우선 사용하고 비K8s 실행 환경에서는 hostname으로 fallback합니다.

### 6.3 트레이스

OpenTelemetry SDK로 계측하고, `traceparent` 헤더(W3C 표준)로 서비스 간 `trace_id`를 전파합니다.

- HTTP/gRPC: OTel 미들웨어(`otelhttp`, `otelgrpc`)가 자동 스팬 생성
- PostgreSQL/MongoDB: 커스텀 래퍼로 개별 쿼리 스팬 기록
- Redis: `redisotel.InstrumentTracing`으로 명령별 자동 스팬 생성
- 10% 샘플링으로 저장 비용과 부하 최소화
- 헬스체크(`/health`, `/ready`), 메트릭 엔드포인트 스팬은 Alloy 수집 단계에서 필터링

### 6.4 프로파일

`pyroscope-go`가 런타임 프로파일을 주기적으로 Pyroscope 서버에 직접 push하여 코드 레벨 병목을 파악합니다 (Alloy를 거치지 않음).

- CPU: 실행 시간을 점유하는 함수 식별
- 메모리: 현재 점유 중인 객체 수와 크기 (Inuse)
- 고루틴: 고루틴을 생성하는 함수의 스택트레이스

### 6.5 통합 조회

Grafana에서 6종의 대시보드를 프로비저닝합니다.

| 대시보드 | 내용 |
| :--- | :--- |
| Overview | 서비스 상태, 전체 시스템 헬스 |
| Traffic | HTTP/gRPC 요청률, 지연, 에러율 |
| Database | PostgreSQL, MongoDB 쿼리 성능 |
| Message | WebSocket 메시지 흐름, 저장, 재시도 |
| Runtime | Go 런타임 (GC, 메모리, 고루틴) |
| Infra | 컨테이너 CPU/메모리 사용량 (cAdvisor) |

`trace_id`를 기준으로 Loki 로그 → Tempo 트레이스 간 자동 연결이 설정되어 있어, 로그에서 트레이스로, 트레이스에서 로그로 즉시 이동할 수 있습니다.

---

## 7. 추후 개선사항

현재 시스템은 Kubernetes dev/test overlay 기준으로 실행됩니다. Docker Compose 실행 경로는 mainline에서 제거했고, Compose 기반 성능/구조 비교가 필요할 때는 `legacy-compose-baseline` tag를 참조합니다. K8s 전환을 위한 앱 코드 변경(동적 멤버십, gRPC round_robin, readiness, graceful drain)과 기본 매니페스트는 반영되어 있으며, 남은 작업은 멀티 노드 HPA/load/rollout 검증과 운영성 고도화 중심입니다.

### 7.1 멀티 노드 K8s 검증

Mac/Windows 호스트의 Linux VM을 Tailscale로 연결하고, k3s 기반 멀티 노드 클러스터에서 실제 네트워크 지연과 노드 장애 조건을 포함해 검증합니다. user-service와 chat-service는 Headless Service + gRPC `dns:///` + `round_robin`으로 멀티 파드 IP를 보게 합니다.

### 7.2 K8s 분산 동작 검증

k3s `qa` overlay에서 HPA 스케일아웃·스케일인, rolling update, `kubectl delete pod`, node drain 시나리오를 검증합니다. 특히 WebSocket owner rebalance 중 in-flight 메시지가 저장 ack 이후 close되는지, gRPC round_robin이 user/chat 다중 파드로 분산되는지, HPA scale-up 후 새 user/chat Pod가 각 client의 subchannel 목록에 들어오기까지 지연이 어느 정도인지 확인합니다.

### 7.3 Custom Metrics HPA

초기 HPA는 CPU 기준으로 시작하되, websocket-service는 활성 연결 수, hub 수, broadcast/persist queue depth 같은 custom metric 기반으로 전환하는 편이 더 정확합니다.

### 7.4 프로덕션 보안

현재 `local-secret.yaml`에 들어 있는 값은 로컬 dev/test를 쉽게 띄우기 위한 샘플입니다. Git에 올라가도 되는 값은 이런 샘플 값뿐입니다.

운영용 JWT secret, 내부 통신 secret, DB 비밀번호 같은 실제 credential은 YAML 파일에 평문으로 적어서 Git에 올리면 안 됩니다. Kubernetes Secret도 기본적으로는 base64 인코딩된 API object라서, 그것만으로 안전한 비밀 저장소라고 보면 안 됩니다.

운영으로 확장한다면 Secret 값은 Git 밖의 안전한 저장소나 배포 파이프라인에서 주입하는 방향으로 분리해야 합니다. 그때 어떤 도구를 쓸지는 실제 운영 환경에 맞춰 정하고, 접근 권한, 암호화, 주기적 교체 절차를 함께 설계합니다.

또한, 처리율 제한의 클라이언트 IP 추출도 Trusted Proxy 기반 파싱으로 전환이 필요합니다.
