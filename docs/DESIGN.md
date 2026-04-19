# 시스템 설계 문서: Go Chat MSA

## 목차

1. [아키텍처 개요](#1-아키텍처-개요)
2. [시스템 상세 설계](#2-시스템-상세-설계)
3. [주요 의사결정](#3-주요-의사결정)
4. [테스트 전략](#4-테스트-전략)
5. [관측성](#5-관측성)
6. [추후 개선사항](#6-추후-개선사항)

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
  - 컨테이너: `testcontainers/testcontainers-go`
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
| api-gateway | REST API 진입점, 인증 위임, 버전 라우팅 | REST | - |
| ws-gateway | WebSocket L7 리버스 프록시, Consistent Hashing | HTTP | - |
| websocket-service | 실시간 메시지 브로드캐스트, 세션/룸 관리 | WebSocket | - |
| user-service | 사용자 및 채팅방 CRUD, Bcrypt 워커 풀 | gRPC | PostgreSQL |
| chat-service | 메시지 저장 및 조회 | gRPC | MongoDB |
| retention-worker | 소프트 삭제된 채팅방 퍼지 | - | PostgreSQL |

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

Access Token 재발급에 사용됩니다. JWT와 달리 토큰 자체에 정보를 담을 필요가 없으므로 UUID로 생성한 opaque token을 사용합니다. Opaque token은 토큰만 봐서는 누구의 것인지, 언제 만료되는지 알 수 없고 반드시 DB 조회가 필요합니다.

DB 유출에 대비하여 원본이 아닌 SHA-256 해시만 저장합니다.

전달은 `HttpOnly`, `SameSite=Strict` 쿠키를 사용하여 XSS/CSRF를 방어합니다. (운영 환경에서는 `Secure` 추가)

현재 프론트엔드는 Nginx 리버스 프록시를 통해 API를 호출하므로 same-origin이며 CORS가 개입하지 않습니다. 프론트엔드가 API를 직접 호출하는 cross-origin 구조로 전환할 경우, `Access-Control-Allow-Origin`에 특정 오리진을 지정하고 `Access-Control-Allow-Credentials`를 설정해야 쿠키 전송이 가능합니다.

토큰 탈취에 대비하여 Refresh Token Rotation을 적용합니다.

1. 사용 시 해당 토큰을 `used=true`로 마킹하고 새 토큰을 발급합니다.
2. 이미 사용된 토큰이 다시 들어오면 탈취로 간주하고, 해당 유저의 모든 Refresh Token을 파기합니다.
3. 만료된 토큰은 백그라운드 고루틴이 주기적으로 일괄 삭제합니다.

#### WebSocket 티켓

WebSocket은 `Authorization` 헤더를 지원하지 않아 URL 쿼리 파라미터로 인증 정보를 전달해야 합니다. JWT를 직접 URL에 노출하면 서버 로그, 브라우저 히스토리 등에 토큰이 남는 보안 위험이 있습니다.

이를 피하기 위해 연결 전에 UUID opaque token으로 30초 TTL의 일회성 티켓을 발급합니다. 티켓은 사용 즉시 삭제(`GetAndDelete`)되며, 만료 시 타이머가 자동으로 제거합니다.

현재 in-memory 저장이라 ws-gateway가 단일 인스턴스일 때만 유효하며, 수평 확장 시 공유 저장소가 필요합니다.

#### 내부 통신 시크릿

ws-gateway는 공개 엔드포인트(`/ws/ticket`, `/ws`)와 내부 엔드포인트(`/internal/*`)를 같은 포트에서 서빙합니다. 네트워크 레벨에서 분리되어 있지 않으므로, api-gateway가 ws-gateway의 내부 API를 호출할 때 `X-Internal-Secret` 헤더로 요청의 출처를 검증합니다. 시크릿은 환경설정으로 주입하는 고정 문자열(pre-shared key)입니다.

시크릿 비교 시 타이밍 공격을 방지하기 위해 `crypto/subtle.ConstantTimeCompare`를 사용합니다.

현재 api-gateway와 ws-gateway 두 서비스만 정적 시크릿을 공유합니다. 추후 내부 통신 보안을 더 강화하려면 mTLS 도입 등이 필요합니다.

### 2.3 처리율 제한 전략

무차별 대입, API 오남용, 도배 등을 방어하기 위해 Token Bucket 알고리즘 기반의 처리율 제한을 적용합니다. 초과 시 429(Too Many Requests)로 거부합니다. 내부 통신 경로(`/internal/*`)는 제한 대상에서 제외합니다.

#### HTTP 미들웨어 (api-gateway, ws-gateway)

| 정책 | 기준 키 | 방어 목적 | RPS / Burst | TTL |
| :--- | :--- | :--- | :--- | :--- |
| 전역 익명 | Client IP | 인증 전 무차별 대입 방어 | 5 / 10 | 10m |
| 인증 사용자 | User ID | API 오남용 방어 | 10 / 20 | 1h |
| 연결 수립 | User ID | 핸드셰이크 단계 세션 점유 방어 | 2 / 5 | 1h |

익명 정책의 클라이언트 IP는 `X-Forwarded-For` 헤더의 첫 번째 값에서 추출합니다. (운영 환경에서는 Trusted Proxy 기반 파싱 필요)

#### WebSocket 세션 (websocket-service)

| 정책 | 기준 키 | 방어 목적 | RPS / Burst | TTL |
| :--- | :--- | :--- | :--- | :--- |
| 세션 내부 | User ID + Room ID | 도배 억제 | 2 / 5 | 1h |

HTTP 미들웨어가 아닌 WebSocket `readPump` 안에서 메시지 단위로 동작합니다.

#### Token Bucket 구현

키 타입은 `string`으로 통일하고, 호출자가 용도에 맞게 키를 포맷합니다. (Client IP, User ID, `userID:roomID` 등)

단일 락 병목을 피하기 위해 `hash/maphash`로 키를 해싱하여 64개 샤드에 분배하고, 샤드별 `sync.Mutex`로 락 경합을 줄입니다. 비활성 버킷은 TTL 기반으로 주기적으로 정리합니다.

### 2.4 데이터 모델 및 인덱스 전략

#### 저장소 선택

사용자/채팅방은 관계가 중요하고(방장, 멤버십, 참조 무결성), 채팅 메시지는 단순 append 위주에 스키마 변경 가능성이 높습니다. 관계형 데이터에는 PostgreSQL, 메시지 저장에는 MongoDB를 사용하여 각 특성에 맞는 저장소를 선택했습니다.

#### PostgreSQL (User Service)

사용자, 채팅방, 멤버십, 리프레시 토큰을 관리합니다.

| 테이블 | 용도 |
| :--- | :--- |
| users | 사용자 계정 |
| rooms | 채팅방 (소프트 삭제용 `deleted_at` 포함) |
| room_members | 채팅방 멤버십 (복합 PK: `user_id`, `room_id`) |
| refresh_tokens | 리프레시 토큰 해시 저장 |

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
| `refresh_tokens.token_hash` | INDEX | 토큰 검증 |
| `refresh_tokens.user_id` | INDEX | 유저별 토큰 일괄 삭제 |

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
| 채팅방 나가기 | 방장 위임 경쟁 조건 방지, 빈 방 자동 soft delete | `rooms` 행 `FOR UPDATE` |
| 토큰 갱신 | 토큰 재사용 탐지 우회 방지 | `refresh_tokens` 행 `FOR UPDATE` |

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

### 2.6 세션 생명주기

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
2. User Service가 해당 방 레코드에 `deleted_at`을 기록합니다.
3. API Gateway가 비동기로 WS Gateway에 요청을 보내 해당 방의 모든 세션을 강제 종료합니다.
4. 이후 해당 방은 검색이나 조회에서 제외됩니다.

### 2.7 메시지 흐름

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

### 2.8 설정 관리

공통 설정 타입은 `internal/shared/config` 패키지에서 일괄 정의하고, 각 서비스는 이를 가져다 조합합니다.

- **공통 타입 (`shared/config`)**: `HTTPServerConfig`, `JWTConfig`, `RateLimitConfig` 등 여러 서비스가 공유하는 설정 빌딩블록. validate 태그를 포함하여 로딩 시점에 검증합니다.
- **서비스별 Config**: 공통 타입을 임베딩(`mapstructure:",squash"`)하거나 필드로 조합하여 서비스 고유의 설정 트리를 구성합니다. 서비스마다 필드 구성이 다른 래퍼 구조체(예: `RateLimitConfig`, `ServiceRegistry`)는 각 서비스 config 파일에 정의합니다.
- **예외**: `cmd/retention-worker`는 Postgres만 사용하므로 공유 `DBConfig` 대신 자체 `DBConfig`를 정의합니다.

#### 환경 격리

환경별로 설정 파일을 분리합니다.
- `configs/base.yaml`: 기본 설정값
- `configs/dev.yaml`: 개발 환경 설정값 (오버라이드 필수 값 위주)
- `test/e2e/configs/test.yaml`: E2E 테스트용 설정값

#### 상수 vs YAML

환경별 차이 여부와 정책적 변경 가능성을 기준으로 분리합니다.

- **Go 상수**: 환경마다 동일하고 변경 가능성이 거의 없는 값. 채널 버퍼 크기, 배치 사이즈, 최대 메시지 크기, 업그레이더 버퍼 등
- **YAML**: 환경마다 달라지거나 정책적으로 변경될 수 있는 값. 타임아웃, 시크릿, 호스트 주소, 처리율 제한 정책 등

### 2.9 우아한 종료

각 서비스는 `errgroup`으로 종료 순서를 관리하며, HTTP/gRPC 서버의 표준 graceful shutdown을 따릅니다. WebSocket Service는 세션과 영속화 파이프라인 때문에 종료 순서가 가장 정교합니다.

1. 종료 신호를 받으면 HTTP 서버가 새 연결 수신을 중단하고 진행 중인 요청 완료를 기다립니다.
2. Manager가 모든 Hub에 종료를 전파합니다. 각 Hub는 세션의 남은 메시지를 밀어낸 후 소켓을 닫습니다.
3. Manager가 영속화 채널을 닫고, 배치 워커가 큐에 남은 메시지를 전부 DB에 반영할 때까지 기다립니다.
4. gRPC 연결과 텔레메트리 수집기를 정리합니다.

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
저장 파이프라인은 배치 워커 풀로, 핸드셰이크 경로는 ws-gateway rate limiter로 동시 호출 수를 억제해 HTTP/2 권장치(연결당 ~100 스트림) 이내로 유도합니다.  
추후 권장치를 초과하는 병목이 관측되면 커넥션 풀을 도입할 예정입니다.

### 3.2 WebSocket 라우팅

WebSocket Service를 다중 인스턴스로 운영할 때, 같은 방의 메시지를 모든 참여자에게 전달하는 방법이 필요합니다. 두 가지 접근이 있습니다.

- **Redis Pub/Sub**: 방 참여자가 여러 노드에 흩어져도 Redis가 중계. 노드 추가/제거가 자유롭지만, 메시지마다 Redis 왕복이 발생하고 외부 인프라 의존 발생
- **Consistent Hashing**: 같은 방의 모든 세션을 한 노드에 모아 인메모리 브로드캐스트. Redis 의존 없이 지연이 낮지만, 노드 변동 시 리밸런싱 비용이 발생

이 프로젝트는 지연 최소화와 구조 단순화를 우선하여 Consistent Hashing을 선택했습니다.

WS Gateway가 `room_id` 기반 Consistent Hashing으로 대상 WebSocket Service 노드를 결정합니다. 일반 해시(`hash(roomID) % nodeCount`)는 노드가 추가되거나 제거될 때 모든 키의 할당이 뒤바뀌어 전체 재연결이 필요하지만, Consistent Hashing은 영향받는 키가 `1/N` 수준으로 최소화됩니다. Consistent Hashing은 결정적이므로 같은 노드 목록이면 어떤 Gateway 인스턴스에서도 동일한 결과가 보장됩니다.

### 3.3 WebSocket 계층 구조

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

Manager와 Hub는 Actor 모델을 따릅니다. 각각 단일 고루틴의 `select` 루프에서 상태 변경을 순차 처리하고, 외부와는 채널로만 통신하므로 뮤텍스 없이 동시성 안전합니다.

#### 계층별 책임

| 계층 | 책임 |
| :--- | :--- |
| Router | HTTP 요청 수신, WebSocket 업그레이드, 멤버십 검증 |
| Manager | Hub 생명주기, 영속화 워커 풀, 처리율 제한기 |
| Hub | Session 생명주기, 브로드캐스트 |
| Session | 개별 연결의 송수신 |

#### 역참조 차단

자식이 부모를 직접 참조하면 순환 의존이 생깁니다. 상향 통신이 필요한 경우 자식이 부모의 존재를 모르도록 우회합니다.

- **송신 전용 채널**: 부모의 채널 참조만 보유하여 값 전달 (Session → Hub 브로드캐스트, Hub → Manager 영속화)
- **콜백 함수**: 부모가 정의한 함수를 클로저로 감싸 자식에게 주입 (Manager의 처리율 제한기 → Session)
- **인터페이스 추상화**: 구현체가 아닌 인터페이스에 의존 (Router의 메시지 저장소 구현체 → Hub)

### 3.4 Bcrypt 워커 풀

Bcrypt는 brute force 방어를 위해 높은 연산 비용을 요구하는 해시 알고리즘입니다. 제한 없이 고루틴을 생성하면 CPU 경합이 증가하고, 해싱 작업보다 컨텍스트 스위칭에 시간을 소비하면서 개별 요청의 레이턴시가 증가합니다. 워커 풀로 동시 해싱 수를 코어 수로 제한하여 이를 방지합니다.

- 워커 수를 `runtime.NumCPU()`로 고정하여 CPU 바운드 작업의 동시성을 제한
- 대기열이 꽉 차면 즉시 `ErrQueueFull`을 반환해 연쇄 장애 방지

### 3.5 비동기 배치 저장

메시지를 브로드캐스트와 동시에 저장하면, 저장 지연이 브로드캐스트 처리량에 영향을 줍니다. 브로드캐스트와 저장을 분리하여, 실시간 전송은 즉시 처리하고 저장은 배치 워커 풀을 통해 비동기로 처리합니다.

- 고정 수의 워커가 채널에서 메시지를 꺼내 일정 건수 단위로 배치 저장
- 배치가 차지 않아도 타이머가 주기적으로 플러시
- 애플리케이션에서 순서를 부여하므로 워커 간 DB 저장 순서는 무관
- 서버 종료 시 채널과 큐에 남은 배치를 모두 플러시하여 유실 최소화

### 3.6 PK로 UUID v7 앱 생성

PK는 애플리케이션에서 UUID v7로 생성합니다. v4 대신 v7을 선택하고, DB 생성 대신 앱 생성을 선택한 이유는 다음과 같습니다.

- v4는 B-tree 전체에 랜덤 삽입되어 디스크로 내려간 페이지를 다시 읽는 캐시 미스가 빈번하지만, v7은 시간 순 단조 증가로 끝에 순차 삽입되어 작업 페이지가 메모리에 유지되므로 캐시 히트율이 높음
- auto-increment와 유사한 삽입 패턴이면서도 외부 노출 시 전체 레코드 수나 생성 속도를 추측할 수 없어 열거 공격에 안전
- INSERT 전에 ID가 확정되어 DB 왕복 없이 관련 엔티티의 FK를 즉시 설정 가능
- 비동기 배치 저장 시에도 ID가 이미 존재하므로 브로드캐스트와 영속화를 독립적으로 진행 가능
- 타임스탬프가 생성 시점 기준이므로, 네트워크 지연이나 재시도로 DB 도달 순서가 바뀌어도 원래 발생 순서가 ID에 보존

---

## 4. 테스트 전략

### 4.1 테스트 구성

| 구분 | 빌드 태그 | 파일 네이밍 | 함수 네이밍 | 실행 방식 |
| :--- | :--- | :--- | :--- | :--- |
| 단위 테스트 | 없음 | `*_test.go` | `TestStruct_Method`, `TestFunc`, `Test_func` | 테이블 기반, `t.Parallel()` |
| 통합 테스트 | `//go:build integration` | `*_integration_test.go` | `(s *Suite) TestMethod_Scenario` | testify/suite, 순차 실행 |
| E2E 테스트 | `//go:build e2e` | `test/e2e/*_test.go` | `(s *E2ESuite) TestScenario_##_Name` | testify/suite, 순차 실행 |

### 4.2 작성 원칙

#### 단위 테스트

- 외부 의존성을 모두 모킹
- 테이블 기반 서브테스트(`t.Run`)로 정의하여 `t.Parallel()` 병렬 실행
- 서브테스트 네이밍: `Success: 설명` / `Failure: 설명 (에러코드)`

#### 통합 테스트

- Testcontainers로 실제 DB를 띄우고 시나리오 위주로 검증
- 데이터 오염 방지를 위해 순차 실행, 매 테스트마다 데이터 초기화

#### E2E 테스트

- Testcontainers로 전체 시스템을 띄우고 블랙박스 검증 (`docker-compose.test.yaml`은 컨테이너 설정 참조용)
- 시나리오 번호 순서대로 사용자 여정(user journey)을 이어가며 검증

---

## 5. 관측성

운영 중인 시스템의 내부 상태를 외부에서 파악하려면 관측성이 필요합니다. 특히 MSA에서는 장애 지점이 여러 서비스에 걸칠 수 있어 더 중요합니다. 로그, 메트릭, 트레이스, 프로파일 4가지 신호를 조합하여 이상 감지(메트릭) → 구간 특정(트레이스) → 상세 컨텍스트(로그) → 코드 레벨 병목(프로파일)을 연결합니다.

계측은 벤더 중립적이고 Go 생태계에서 사실상 표준인 OpenTelemetry SDK를 사용합니다. 백엔드를 교체해도 애플리케이션 코드 변경이 필요 없습니다. 백엔드는 Grafana 스택(Loki, Prometheus, Tempo, Pyroscope)으로 통일하여 4가지 신호를 하나의 대시보드에서 통합 조회합니다.

로그, 메트릭, 트레이스는 Grafana Alloy(OTel Collector)가 수집하여 각 백엔드로 라우팅합니다. 프로파일은 `pyroscope-go`가 Pyroscope 서버에 직접 push합니다.

| 신호 | 백엔드 | 용도 |
| :--- | :--- | :--- |
| 로그 | Loki | 이벤트 기록 검색 |
| 메트릭 | Prometheus | 이상 감지 |
| 트레이스 | Tempo | 서비스 간 요청 흐름 추적 |
| 프로파일 | Pyroscope | 코드 레벨 병목 분석 |

### 5.1 로그

`slog`로 JSON 구조화 로깅을 하며, 모든 로그에 `trace_id`와 `span_id`를 자동 주입하여 트레이스와 연결합니다.

- 에러 로그는 발생 지점이 아닌 호출 스택 최상단에서 한 번만 기록
- HTTP 로그에서 민감한 쿼리 파라미터(token, password, secret 등)를 자동 마스킹
- 헬스체크 로그는 Alloy 수집 단계에서 필터링

### 5.2 메트릭

OTel Metrics SDK로 계측하고 OTLP로 Alloy에 push합니다. Alloy가 Prometheus에 remote write합니다. 커스텀 미들웨어/인터셉터/래퍼로 계측합니다.

- HTTP: 요청 수, 지연 시간, 상태 코드
- gRPC: 서버/클라이언트 양쪽 요청 수, 지연 시간, 상태 코드
- DB: 쿼리 수, 지연 시간 (PostgreSQL, MongoDB 래퍼)
- WebSocket: 세션, 메시지, 저장 파이프라인 지표
- User: 인증, Bcrypt 워커 풀 지표
- Chat: 메시지 저장, 조회 지표

고카디널리티 방지를 위해 URL 경로의 UUID를 `:id`로 정규화하고, 헬스체크/메트릭 엔드포인트는 수집에서 제외합니다.

### 5.3 트레이스

OpenTelemetry SDK로 계측하고, `traceparent` 헤더(W3C 표준)로 서비스 간 `trace_id`를 전파합니다.

- HTTP/gRPC는 OTel 미들웨어(`otelhttp`, `otelgrpc`)가 스팬을 자동 생성
- DB 쿼리는 커스텀 래퍼로 개별 스팬 기록
- 10% 샘플링으로 저장 비용과 부하 최소화
- 헬스체크, 메트릭 엔드포인트 스팬은 Alloy 수집 단계에서 필터링

### 5.4 프로파일

`pyroscope-go`가 런타임 프로파일을 주기적으로 Pyroscope 서버에 직접 push하여 코드 레벨 병목을 파악합니다 (Alloy를 거치지 않음).

- CPU: 실행 시간을 점유하는 함수 식별
- 메모리: 현재 점유 중인 객체 수와 크기 (Inuse)
- 고루틴: 고루틴을 생성하는 함수의 스택트레이스

### 5.5 통합 조회

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

## 6. 추후 개선사항

현재 시스템은 Docker Compose 기반 정적 인프라 환경에서 동작합니다. k8s 등의 동적 환경으로 확장하고 시스템 고도화를 위해 필요한 개선사항을 정리합니다.

### 6.1 서비스 디스커버리

WebSocket 노드 목록이 설정 파일에 하드코딩되어 있어 노드 추가/제거 시 재배포가 필요합니다. 서비스 레지스트리나 k8s 서비스 디스커버리를 도입하여 노드 변동을 자동 반영할 예정입니다.

### 6.2 Consistent Hashing 리밸런싱

현재는 노드 목록이 고정이라 런타임에 Consistent Hashing 링 변경이 불가합니다. 서비스 디스커버리 도입 후 동적 노드 변동이 가능해지면, 같은 방을 한 노드에 모아야 하므로 영향받는 방의 세션을 닫고 클라이언트 자동 재연결로 올바른 노드에 재배치하는 리밸런싱 전략이 필요합니다.

### 6.3 가중치 기반 부하 분산

Room ID 해시만으로 노드를 결정하므로 방 규모에 따른 부하 편차가 발생할 수 있습니다. 노드별 할당된 방들의 최대 인원 합산값을 가중치로 활용하여 부하를 균등화할 예정입니다.

### 6.4 프로덕션 보안

프로덕션용 JWT 시크릿과 내부 통신 시크릿을 YAML 파일이 아닌 환경변수로 주입해야 합니다. 또한, 처리율 제한의 클라이언트 IP 추출도 Trusted Proxy 기반 파싱으로 전환이 필요합니다.
