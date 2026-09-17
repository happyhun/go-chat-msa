# 시스템 설계 문서: Go Chat MSA

## 목차

1. [아키텍처 개요](#1-아키텍처-개요)
2. [시스템 상세 설계](#2-시스템-상세-설계)
3. [주요 의사결정](#3-주요-의사결정)
4. [Kubernetes 배포 설계](#4-kubernetes-배포-설계)
5. [테스트 전략](#5-테스트-전략)
6. [관측성](#6-관측성)
7. [검증 범위와 운영 경계](#7-검증-범위와-운영-경계)

---

## 1. 아키텍처 개요

### 1.1 설계 목표

사용자·방 관리, 실시간 연결, 메시지 저장을 각각 확장할 수 있도록 책임을 나눕니다. REST는 인증과 도메인 조회·변경, WebSocket은 채팅 송수신을 담당합니다.

같은 방의 연결은 여러 websocket-service Pod에 분산될 수 있습니다. 각 Pod는 로컬 세션이 있는 방을 NATS로 구독하고, 수신한 메시지를 자기 세션에 전달합니다. 방별 담당 Pod나 전역 순번은 두지 않습니다.

사용자 채팅은 JetStream에 먼저 기록한 뒤 Core NATS RePublish로 전달합니다. chat-service는 durable consumer에서 메시지를 가져와 MongoDB에 저장합니다. MongoDB 장애 중에도 JetStream 용량이 남아 있으면 메시지를 수락할 수 있습니다. 이력 조회와 누락 복구는 MongoDB를 사용합니다.

사용자·방·멤버십은 PostgreSQL, 채팅 이력은 MongoDB, 저장 대기 메시지는 JetStream, 인증·티켓·HTTP 처리율 제한은 Redis에 둡니다. 실행과 검증의 기준은 로컬 kind의 `dev`/`test`/`qa` overlay입니다.

### 1.2 서비스 구성

| 서비스 | 책임 | 통신 | 상태/저장소 |
| :--- | :--- | :--- | :--- |
| api-gateway | REST API, JWT 검증, WebSocket 티켓 발급 | HTTP, gRPC, 내부 HTTP | Redis (티켓, 요청 제한) |
| websocket-service | 연결 검증, 로컬 세션과 Hub, 메시지 발행·구독 | WebSocket, NATS, user gRPC, 내부 HTTP | 로컬 세션, Redis (티켓 소비, 연결 제한) |
| user-service | 사용자·방·멤버십, refresh token | gRPC | PostgreSQL, Redis |
| chat-service | durable consumer 배치 저장, 이력·복구 조회 | NATS JetStream, gRPC | MongoDB |

`ws-gateway`는 제거했습니다. Ingress가 `/ws`를 websocket-service의 ClusterIP Service로 직접 전달합니다.

### 1.3 주요 기술 선택

아래 표는 주요 기술 선택과 그 근거를 정리한 것입니다.

| 영역 | 선택 | 근거 |
| :--- | :--- | :--- |
| 언어/런타임 | Go 1.27 | 연결별 read/write loop를 단순하게 구성하고, 정적 바이너리로 배포를 단순화하기 위함 |
| 외부 API | `net/http`, OpenAPI | 프레임워크 의존을 줄이고, OpenAPI 명세로 요청/응답 계약을 먼저 고정하기 위함 |
| 내부 통신 | gRPC, Buf | 서비스 간 계약을 `.proto`로 명확히 정의하고, 생성 코드로 호출부 불일치를 줄이기 위함 |
| 메시지 브로커 | NATS Core + JetStream | Pod 간 실시간 fan-out과 MongoDB 저장 전 durable backlog를 분리 |
| WebSocket | `gorilla/websocket` | 표준 라이브러리의 HTTP 서버 위에서 WebSocket handshake와 frame 처리를 다루기 위함 |
| 저장소 접근 | `pgx`, `mongo-driver`, `go-redis` | 각 저장소의 커넥션 풀, 명령, 계측을 드라이버 특성에 맞게 다루기 위함 |
| 인증/보안 | `golang-jwt/jwt/v5`, `x/crypto` | 검증된 패키지로 JWT 서명 검증과 bcrypt 해시를 처리해 직접 구현 위험을 줄이기 위함 |
| 설정/검증 | `viper`, `validator` | 환경별 설정을 주입하고, 잘못된 설정은 애플리케이션 시작 시점에 차단하기 위함 |
| 테스트/실행 | `testify`, `testcontainers`, kind | 단위, 통합, 로컬 Kubernetes 검증을 분리해 실패 원인을 좁히기 위함 |
| 관측성 | OpenTelemetry, Grafana Stack | 벤더 중립 계측을 유지하면서 로그, 메트릭, 트레이스, 프로파일을 함께 보기 위함 |
| 생성/마이그레이션 | sqlc, golang-migrate, mockery | 반복 코드를 줄이고 DB 변경과 mock 생성을 재현 가능하게 관리하기 위함 |

---

## 2. 시스템 상세 설계

상세 설계에서는 API 계약, 인증 상태, 메시지 흐름처럼 시스템 동작을 이해하는 데 필요한 항목을 설명합니다. 각 항목은 상태를 어디에 두는지, 실패했을 때 어떤 범위까지 막는지에 초점을 둡니다.

### 2.1 API 설계

스키마를 먼저 정의하고 코드를 생성하는 API-first 방식을 따릅니다. 외부 REST는 클라이언트와의 공개 계약이고, 내부 gRPC는 서비스 사이의 호출 계약입니다.

#### 외부 REST

외부 REST는 [Zalando RESTful API Guidelines](https://opensource.zalando.com/restful-api-guidelines/)를 기준으로 설계합니다. 경로는 리소스 명사 중심으로 두고, 요청/응답 계약은 [OpenAPI 명세](../api/openapi/openapi.yaml)에 먼저 정의합니다.

| 항목 | 기준 |
| :--- | :--- |
| 경로 | 동작이 아니라 리소스를 표현합니다. 예: `POST /rooms`, `GET /rooms/{id}/messages` |
| 필드명 | 쿼리 파라미터와 JSON 필드는 `snake_case`로 통일합니다. 예: `room_id`, `created_at` |
| 에러 | 오류 응답은 HTTP API 표준 형식인 Problem Details(RFC 9457)를 사용하며, `type`, `title`, `status`, `detail`을 포함합니다. |
| 처리율 제한 | 요청이 허용량을 넘으면 HTTP 429 Too Many Requests를 반환합니다. 응답의 `Retry-After` 헤더로 클라이언트가 언제 다시 시도할 수 있는지 알려줍니다. |

#### 내부 gRPC

내부 gRPC는 [Google API Design Guide](https://cloud.google.com/apis/design)를 기준으로 설계합니다. 서비스 간 호출 계약은 [api/proto/](../api/proto/)의 `.proto` 파일에 먼저 정의하고, 생성된 Go 코드로 서버와 클라이언트 타입을 맞춥니다.

| 항목 | 기준 |
| :--- | :--- |
| 계약 | 요청/응답 메시지와 서비스 메서드를 `.proto`에 정의합니다. 예: `BatchGetUsersRequest`, `UserService.BatchGetUsers` |
| 메서드 | 서비스 간 호출 의도가 드러나도록 동사 중심으로 이름을 붙입니다. 예: `BatchGetUsers`, `ListMessages`, `SyncMessages` |
| 에러 | 비즈니스 오류는 gRPC 상태 코드로 표현합니다. 예: `InvalidArgument`, `PermissionDenied`, `NotFound` |
| 생성 코드 | proto 변경 후 서버/클라이언트 코드를 생성해 메서드 시그니처와 메시지 필드 접근 오류를 컴파일 시점에 드러냅니다. |

#### 요청 검증 책임

요청 검증은 두 단계로 나눕니다. Gateway는 HTTP 요청을 해석하는 데 필요한 형식과 기본 범위를 먼저 확인하고, 도메인 데이터를 소유한 서비스가 최종 규칙을 검증합니다.

| 위치 | 책임 | 예시 |
| :--- | :--- | :--- |
| REST 핸들러 | HTTP 요청 해석과 기본 검증 | JSON 파싱 실패, 필수 필드 누락, `limit`/`offset` 범위 |
| gRPC 서비스 | 도메인 규칙과 최종 검증 | username 길이, password 정책, 방 정원 `capacity` |

### 2.2 인증 전략

#### Access Token

access token은 짧은 수명의 JWT입니다. 클라이언트는 `Authorization: Bearer` 헤더로 전달하고, 외부 요청의 인증 경계를 담당하는 api-gateway가 토큰의 서명, `user_id`, `username`, 만료 시간을 검증합니다. 토큰을 발급하는 user-service와 검증하는 gateway가 같은 내부 시크릿을 공유하는 구조이므로, 공개키 배포가 필요한 비대칭 서명보다 HS256이 단순합니다. access token은 별도 폐기 상태를 두지 않고, 수명을 짧게 가져가 탈취되더라도 유효 시간을 제한합니다.

#### Refresh Token

refresh token은 access token을 재발급하기 위한 토큰입니다. 사용자 정보를 담지 않는 UUID 기반 opaque token으로 발급해, 토큰 값만으로는 사용자나 만료 시간을 알 수 없게 합니다. 서버는 토큰 원문을 저장하지 않고 SHA-256으로 해시한 값만 저장하므로, 저장소가 유출되더라도 실제 토큰 값을 알 수 없습니다.

refresh token의 유효 상태는 PostgreSQL이 아니라 Redis에 둡니다. 사용자 계정처럼 복구와 관계 무결성이 중요한 데이터가 아니라, 만료와 갱신이 반복되는 인증 상태이기 때문입니다. 만료 정리는 Redis TTL에 맡기고, token rotation은 Lua 스크립트로 원자적으로 처리합니다.

refresh token이 탈취되더라도 같은 토큰을 계속 사용할 수 없도록 token rotation을 적용합니다.

1. 로그인 성공 시 발급한 refresh token을 현재 유효한 토큰으로 등록하고 만료 시간을 함께 둡니다.
2. refresh 요청이 들어오면 user-service가 새 refresh token을 먼저 발급합니다. Redis에서는 기존 active 키를 삭제하고, 기존 토큰의 used 키와 새 토큰의 active 키를 같은 Lua 스크립트에서 생성합니다.
3. 이미 사용 완료된 토큰이 다시 들어오면 재사용 공격으로 보고 해당 사용자의 유효한 refresh token을 모두 폐기합니다.

로테이션과 별도로, 사용자가 세션을 끝내는 경우에는 토큰을 즉시 폐기합니다. 로그아웃은 현재 요청에 사용된 refresh token만 폐기하고, 회원탈퇴는 해당 사용자의 모든 refresh token을 폐기합니다.

브라우저에는 refresh token을 `HttpOnly`, `SameSite=Strict` 쿠키로 전달합니다. `HttpOnly`는 자바스크립트에서 토큰을 읽지 못하게 해 XSS 피해를 줄이고, `SameSite=Strict`는 다른 사이트에서 시작된 요청에 쿠키가 자동으로 실리는 상황을 막아 CSRF 위험을 줄입니다. Kubernetes에서는 ingress-nginx, 로컬 프론트엔드 개발에서는 Vite가 `/api`를 api-gateway로 프록시하므로 same-origin 경로에서 인증 쿠키를 다룹니다. 프론트엔드 컨테이너의 Nginx는 정적 파일만 제공합니다. 운영 환경에서는 HTTPS에서만 쿠키가 전송되도록 `Secure` 속성을 추가해야 합니다.

Redis 장애로 refresh token 상태를 확인하거나 갱신할 수 없으면 로그인, 토큰 재발급, 로그아웃 요청은 실패시킵니다. 유효성을 확인할 수 없는 토큰을 허용하지 않기 위해서입니다.

로그인부터 WebSocket 티켓 발급까지의 흐름은 [로그인과 WebSocket 인증 시퀀스](diagrams/seq-auth-ticket.mmd)에 정리했습니다.

#### WebSocket 티켓

브라우저 WebSocket API는 `Authorization` 헤더를 직접 지정할 수 없습니다. 그래서 연결 요청에는 URL 쿼리 파라미터를 써야 하는데, JWT를 그대로 넣으면 서버 로그나 브라우저 히스토리에 토큰이 남을 수 있습니다.

이를 피하기 위해 WebSocket 연결 전에 30초 TTL의 일회성 티켓을 발급합니다. 티켓은 UUID 기반 opaque token이며, Redis의 `ws:ticket:{uuid}` key에 저장됩니다. 연결 시에는 이 티켓을 원자적으로 소비하므로 같은 티켓을 다시 사용할 수 없습니다.

api-gateway의 `POST /auth/ws-ticket`에서 발급하고 websocket-service가 연결 시 Redis `GETDEL`로 소비합니다. 외부 경로는 `POST /api/auth/ws-ticket`, `GET /ws?room_id=...&ticket=...`입니다. 티켓 소비 후 방 멤버십을 확인하며, 연결에 실패하면 다음 시도에서 새 티켓을 받습니다.

#### 내부 통신 시크릿

api-gateway는 websocket-service의 내부 HTTP API로 시스템 메시지 발행과 방 세션 종료를 요청합니다. 대상은 `/internal/rooms/{id}/system-messages`와 `/internal/rooms/{id}/sessions`이며 `X-Internal-Secret`을 `crypto/subtle.ConstantTimeCompare`로 검증합니다.

두 서비스가 공유하는 정적 시크릿은 로컬 dev/test/qa Secret에서 주입합니다. Ingress는 `/internal/*`를 외부로 노출하지 않습니다. 실제 인증 정보는 Git에 두지 않습니다.

### 2.3 처리율 제한 전략

처리율 제한은 요청 성격에 따라 기준을 다르게 둡니다. 공개 API와 WebSocket 연결 요청은 클라이언트 IP로 제한하고, 인증 API와 WebSocket 티켓 발급은 사용자 ID 기준으로 제한합니다. 채팅 메시지는 같은 사용자가 같은 방에 과도하게 보내는 경우를 막습니다.

HTTP 요청은 허용량을 넘으면 HTTP 429 Too Many Requests로 거부하고, `Retry-After` 헤더로 재시도 시점을 알려줍니다. WebSocket 메시지는 연결을 바로 끊지 않고 해당 세션에 제한 경고를 보냅니다. 내부 통신 경로(`/internal/*`)는 외부 사용자가 직접 호출하는 경로가 아니므로 제한 대상에서 제외합니다.

#### HTTP 요청 제한 (api-gateway, websocket-service)

| 대상 | 제한 기준 | 방어 목적 | 기본값 |
| :--- | :--- | :--- | :--- |
| api-gateway 공개 API | 클라이언트 IP | 로그인 시도와 공개 API 남용 방어 | 초당 5회 / 순간 허용 10회 |
| api-gateway 인증 API | 사용자 ID | 로그인 후 API 과다 호출 방어 | 초당 10회 / 순간 허용 20회 |
| websocket-service 연결 | 클라이언트 IP | WebSocket 연결 요청 남용 방어 | 초당 5회 / 순간 허용 10회 |
| api-gateway 티켓 발급 | 사용자 ID | 연결 시도 폭주 방어 | 초당 2회 / 순간 허용 5회 |

IP 기준 제한은 `X-Forwarded-For` 헤더의 첫 번째 값을 사용합니다. 다만 이 헤더는 클라이언트가 임의로 보낼 수 있으므로, 운영 환경에서는 요청의 `RemoteAddr`가 신뢰할 수 있는 프록시 대역에 속할 때만 `X-Forwarded-For`를 사용해야 합니다. 그렇지 않으면 공격자가 헤더 값을 바꿔 IP 기반 제한을 우회할 수 있습니다.

HTTP 요청은 여러 gateway 인스턴스에서 같은 제한 상태를 공유해야 하므로 Redis 기반 `redis_rate/v10`을 사용합니다. Redis 장애 시 HTTP 미들웨어는 요청을 통과시키고 경고 로그를 남깁니다. 과부하 방어 장치의 장애 때문에 정상 API까지 멈추지 않기 위한 선택입니다.

#### WebSocket 메시지 제한 (websocket-service)

| 대상 | 제한 기준 | 방어 목적 | 기본값 | TTL |
| :--- | :--- | :--- | :--- | :--- |
| 메시지 전송 | 사용자 ID + 방 ID | 채팅방 도배 억제 | 초당 2회 / 순간 허용 5회 | 1h |

WebSocket 메시지는 HTTP 미들웨어가 아니라 메시지를 읽는 단계에서 제한합니다. 제한 기준은 사용자 ID와 방 ID 조합이며, 메시지 처리 지연을 줄이기 위해 Redis를 호출하지 않고 인스턴스 메모리에서 검사합니다. 내부적으로는 토큰 버킷을 64개 샤드로 나눠 락 경합을 줄이고, 오래 쓰지 않은 버킷은 TTL 기준으로 정리합니다.

### 2.4 데이터 모델 및 인덱스 전략

#### 저장소 선택

저장소는 데이터의 성격에 맞춰 나눕니다. 사용자, 채팅방, 멤버십은 관계와 제약 조건이 중요하므로 PostgreSQL에 둡니다. 채팅 메시지는 메시지 종류에 따라 필드가 달라질 수 있어 유연한 스키마가 필요하고, 메시지 저장/조회 부하를 관계형 데이터와 분리하기 위해 MongoDB에 저장합니다. Redis는 원본 데이터를 보관하지 않고, TTL과 원자 처리가 필요한 제어 상태만 맡습니다.

#### PostgreSQL (User Service)

PostgreSQL은 사용자, 채팅방, 멤버십을 관리합니다. 이 데이터는 외래키, 유니크 제약, 트랜잭션으로 무결성을 지키는 것이 중요합니다. Refresh Token은 만료되는 인증 상태이므로 PostgreSQL에 두지 않고 Redis에서 관리합니다.

| 테이블 | 용도 |
| :--- | :--- |
| users | 사용자 계정 |
| rooms | 채팅방 |
| room_members | 채팅방 멤버십 (복합 PK: `user_id`, `room_id`) |

사용자와 채팅방 ID는 애플리케이션에서 UUID v7로 생성합니다.

- ID에 생성 시각이 포함되어 시간순 정렬과 로그 추적이 쉽습니다.
- INSERT 전에 ID를 만들 수 있어 서비스 경계를 넘는 요청에서도 같은 식별자를 사용할 수 있습니다.
- UUID v4보다 새 ID가 B-tree 인덱스의 비슷한 위치에 삽입되어 랜덤 삽입 부담을 줄일 수 있습니다.

UUID v7에 생성 시각이 포함되지만, 조회 조건과 운영 중 확인이 쉽도록 `created_at` 컬럼은 별도로 둡니다.

인덱스는 주요 조회 경로에 맞춰 추가합니다.

| 대상 | 종류 | 용도 |
| :--- | :--- | :--- |
| `users.username` | UNIQUE | 로그인, 중복 방지 |
| `room_members.room_id` | INDEX | 방별 멤버 목록 |
| `rooms.manager_id` | INDEX | 방장별 방 조회 |
| `rooms.name` | GIN (`pg_trgm`) | 방 이름 중간 일치 검색(`ILIKE '%keyword%'`) |

현재 방 이름 검색은 단순 키워드 검색이지만, 중간 일치(`ILIKE '%keyword%'`)가 필요합니다. 일반 B-tree 인덱스는 이런 검색에 활용되기 어렵기 때문에 `pg_trgm` 확장과 GIN 인덱스를 사용합니다.

트랜잭션 실행 함수(`runInTx`)는 외부에서 주입합니다. 운영 환경에서는 실제 트랜잭션을 실행하고, 단위 테스트에서는 mock으로 대체해 트랜잭션 경계를 검증합니다.

여러 쿼리가 원자적으로 실행되어야 하거나 조회-수정 사이 경쟁 조건이 발생할 수 있는 연산은 트랜잭션과 `SELECT FOR UPDATE` 행 잠금으로 보호합니다.

| 연산 | 보호 대상 | 잠금 방식 |
| :--- | :--- | :--- |
| 채팅방 참여 | 정원 초과 방지 | `rooms` 행 `FOR UPDATE` |
| 채팅방 생성 | 방 생성 + 방장 멤버 추가 원자성 | 트랜잭션 래핑 |
| 채팅방 수정 | 정원 축소 시 현재 인원 수 검증, 참여/삭제와의 경쟁 조건 방지 | `rooms` 행 `FOR UPDATE` |
| 채팅방 삭제 | 참여와의 경쟁 조건 방지, 방장 권한 검증 원자성 | `rooms` 행 `FOR UPDATE` |
| 채팅방 나가기 | 방장 위임 경쟁 조건 방지, 빈 방 자동 삭제 | `rooms` 행 `FOR UPDATE` |

#### MongoDB (Chat Service)

`messages` 컬렉션에는 사용자 채팅만 저장합니다. 문서는 UUIDv7 `_id`, `roomId`, `senderId`, `clientMsgId`, `type`, `content`, `createdAt`을 가집니다. 시스템 메시지는 실시간 알림이며 저장하지 않습니다.

| 인덱스 | 종류 | 용도 |
| :--- | :--- | :--- |
| `_id` | UNIQUE | 메시지 ID 중복 방지 |
| `{ roomId, senderId, clientMsgId }` | UNIQUE | 같은 방·발신자의 재전송 중복 방지 |
| `{ roomId, _id }` | INDEX | UUIDv7 기반 이력·복구 조회 |
| `{ createdAt }` | TTL (90일) | 오래된 메시지 자동 파기 |

저장은 unordered `InsertMany`와 `w:1, j:true`를 사용합니다. 중복 키 오류는 기존 문서의 방·발신자·클라이언트 ID·종류·본문을 대조해 같은 메시지일 때만 성공으로 처리합니다. 같은 키에 다른 내용이 있으면 영구 충돌입니다. write concern 오류처럼 저장 결과가 불확실하면 재시도합니다.

#### Redis (제어 상태)

| 키 | 값 | 책임 서비스 | 만료/갱신 | 용도 |
| :--- | :--- | :--- | :--- | :--- |
| `auth:rt:active:{tokenHash}` | 사용자 ID | user-service | 토큰 만료 시간 | 유효 refresh token |
| `auth:rt:used:{tokenHash}` | 사용자 ID | user-service | 이전 토큰의 남은 만료 시간 | 재사용 탐지 |
| `auth:rt:user:{userID}` | 유효 토큰 해시 목록 | user-service | 토큰 만료 시간으로 갱신 | 사용자 단위 폐기 |
| `ws:ticket:{uuid}` | 사용자 ID | api-gateway 발급, websocket-service 소비 | 30초, 사용 시 삭제 | 일회성 연결 티켓 |
| `rate:*` | 처리율 제한 상태 | api-gateway, websocket-service | `redis_rate` 관리 | HTTP 요청·연결 제한 |

refresh token rotation은 Lua 스크립트로 원자 처리합니다. Redis에는 방 소유권, 순번, WebSocket Pod 목록을 저장하지 않습니다. 메시지 전송 제한은 Pod 메모리에 있으므로 여러 Pod를 합친 전역 제한은 아닙니다.

#### JetStream (저장 대기 상태)

chat-service가 시작할 때 아래 stream과 shared durable consumer를 생성·갱신합니다. 두 stream 모두 file storage, replica 1, `DiscardNew`이며 메시지 나이에 따른 만료를 설정하지 않습니다.

| 항목 | 기본 설정 |
| :--- | :--- |
| 저장 stream | `CHAT_PERSIST`, subject `chat.persist.*`, WorkQueue retention, 최대 1GiB |
| 중복 수락 억제 | `Nats-Msg-Id`, 2분 window |
| 실시간 전달 | `chat.persist.*` → `room.msg.$1` Core RePublish |
| consumer | `chat-persistence`, explicit ACK, `AckWait=30s`, `MaxDeliver=-1`, `MaxAckPending=8000` |
| DLQ | `CHAT_PERSIST_DLQ`, subject `chat.dlq.persistence`, 최대 128MiB |

stream이 가득 차면 새 메시지 수락이 실패합니다. 기존 backlog를 지우며 새 메시지를 받지 않습니다. DLQ는 자동 재처리하지 않습니다.

#### 스키마 마이그레이션

스키마와 인덱스는 마이그레이션 파일에서만 관리합니다. 실행 코드가 시작할 때 인덱스를 자동 생성하지 않게 해, 환경마다 스키마가 달라지는 상황을 피합니다.

- 개발 환경과 통합 테스트에서 `golang-migrate/migrate` 공통 사용
- 인덱스 생성은 레포지토리 코드에서 제외, 마이그레이션 스크립트에 일임
- 타임스탬프 기반 버전 관리로 파일명 충돌 방지

### 2.5 채팅방 동작

채팅방 참여/나가기는 멤버십을 바꾸는 REST 동작이고, 접속/접속 해제는 WebSocket 연결 상태입니다. 참여·나가기는 Core NATS 시스템 메시지로 알립니다. 이 알림은 채팅 이력에 저장하지 않으며, 화면 진입·연결 종료에는 알림을 만들지 않습니다.

| 동작 | 설명 | 구현 방식 | 시스템 메시지 |
| :--- | :--- | :--- | :--- |
| 참여 | 채팅방 멤버로 등록 | REST API | "OOO님이 들어왔습니다" |
| 나가기 | 채팅방 멤버에서 탈퇴 | REST API | "OOO님이 나갔습니다" |
| 삭제 | 채팅방 삭제 | REST API | 없음 |
| 접속 | 채팅방 화면 진입 | WebSocket 핸드셰이크 | 없음 |
| 접속 해제 | 채팅방 화면 이탈 | WebSocket Close | 없음 |

### 2.6 회원 라이프사이클

현재 프로젝트는 회원탈퇴를 물리 삭제로 처리합니다. 이 채팅 시스템에는 법적 보관이나 결제/정산처럼 탈퇴 후에도 사용자 행을 일정 기간 보존해야 하는 요구사항이 없습니다. 이런 요구 없이 논리 삭제와 유예 기간을 먼저 넣으면, 실제 기능보다 조회 조건과 정리 작업이 먼저 복잡해집니다.

탈퇴 요청은 다음 순서로 처리합니다.

1. 비밀번호를 재검증합니다.
2. Redis에 저장된 해당 사용자의 refresh token을 모두 폐기합니다.
3. 트랜잭션 안에서 가입한 방마다 LeaveRoom 로직을 적용합니다. 사용자가 방장이면 다른 멤버에게 방장을 위임하고, 혼자 있는 방이면 방을 삭제합니다.
4. 같은 트랜잭션 안에서 가입한 방 정리를 마친 뒤 `users` row를 삭제합니다.

물리 삭제를 사용하면 모든 사용자 조회에 `deleted_at IS NULL` 조건을 붙일 필요가 없고, 동일 username도 삭제 직후 다시 사용할 수 있습니다. 대신 삭제 후 복구나 유예 기간은 제공하지 않습니다. 이후 탈퇴 데이터 보관 요구가 생기면 논리 삭제 컬럼과 보관 기간을 함께 설계해야 합니다.

### 2.7 세션 생명주기

연결 준비부터 세션 등록까지는 [WebSocket 연결 시퀀스](diagrams/seq-websocket.mmd), 방 구독의 생성·종료는 [방 구독 생명주기](diagrams/seq-room-subscription.mmd)에 정리했습니다.

1. Ingress가 `/ws` 요청을 준비된 websocket-service Pod로 전달합니다.
2. 연결 처리율 제한을 검사하고 Redis 티켓을 원자적으로 소비합니다.
3. Router가 user-service에 방 멤버십을 확인합니다.
4. `Manager.PrepareRegister`가 로컬 Hub를 찾거나 생성합니다. 새 Hub는 `room.msg.{roomID}`를 일반 Core subscription으로 구독하고 flush 완료를 기다립니다.
5. 준비에 실패하면 WebSocket upgrade 전에 HTTP 오류를 반환합니다. 준비가 끝나면 upgrade 후 `Commit`으로 세션을 등록합니다. upgrade 실패 시 준비를 취소하고, upgrade 이후 `Commit`이 실패하면 준비 취소와 함께 연결을 닫습니다.
6. 각 세션은 고유 ID로 관리하며 같은 사용자의 여러 탭·연결을 허용합니다. 마지막 세션이 나간 Hub는 idle timeout(기본 5분) 후 종료하고 구독을 해제합니다.

방 삭제는 user-service의 DB 변경 후 api-gateway가 내부 HTTP로 세션 종료를 요청합니다. 요청을 받은 Pod가 Core NATS `room.event.{roomID}`에 `room_closed`를 발행하면 각 Pod가 로컬 세션을 닫습니다. 시스템 메시지와 제어 이벤트는 durable 저장·재생 대상이 아닙니다.

DB 삭제 후 내부 HTTP 요청이 실패하면 api-gateway는 로그를 남기고 재시도하지 않습니다. Core NATS 이벤트에도 Pod별 처리 확인과 재전달이 없어, 종료 요청이나 이벤트 전달이 실패하면 일부 Pod에 기존 세션이 남을 수 있습니다. 이 경로의 실패는 websocket-service와 NATS의 연결 단절과 별개이며, NATS 단절 시 세션을 닫는 처리만으로는 복구되지 않습니다.

### 2.8 메시지 흐름

[메시지 처리 흐름](diagrams/flow-message.mmd)은 실시간 전달과 저장 경계를 함께 보여줍니다.

#### 전송과 수락

1. 클라이언트가 `client_msg_id`, `content`를 담은 chat frame을 보냅니다.
2. Session이 사용자·방 단위 처리율 제한, 메시지 종류, 필수 필드와 길이, UUID 형식을 확인합니다.
3. Manager가 UUIDv7 메시지 ID를 만들고 `chat.persist.{roomID}`에 한 번 동기 발행합니다.
4. JetStream은 stream write 성공 뒤 `room.msg.{roomID}`에 Core RePublish합니다. 발행자는 `PubAck` 성공을 수락 기준으로 사용합니다.
5. 같은 방을 구독한 모든 Pod가 로컬 세션에 fan-out합니다. 발신자도 이 경로로 echo를 받습니다.

WebSocket 쓰기 성공은 영속 수락 확인이 아닙니다. RePublish echo는 stream write 성공을 보여주지만, MongoDB 저장 완료나 발신 Pod의 `PubAck` 수신까지 증명하지는 않습니다. 별도의 클라이언트 수락 ACK frame은 없습니다. 발행 실패 시 성공 echo를 직접 만들지 않고 세션을 종료합니다.

Core NATS fan-out에는 Pod별 ACK와 replay가 없습니다. 세션 전송 큐가 가득 차면 프레임을 버릴 수 있으며, 연결별 `frame_no`의 간격으로 이후 프레임에서 누락 가능성을 감지합니다. NATS disconnect, slow consumer, 전달 지연 한도 초과는 해당 세션들을 닫아 재연결·복구를 유도합니다.

#### 멱등성과 정렬

JetStream dedup ID는 방·발신자·`client_msg_id`·종류·본문의 해시입니다. 같은 요청의 2분 이내 재발행을 억제하며, window 이후의 중복은 MongoDB 유니크 인덱스와 문서 대조로 처리합니다.

프론트엔드는 서버 ID와 `(room_id, sender_id, client_msg_id)`로 중복을 제거합니다. 서버 echo는 같은 클라이언트 키의 임시 메시지를 교체하고, UUIDv7 ID를 기준으로 이분 탐색·삽입하거나 조회 배치를 병합합니다. Pod 간 도착 순서와 DB 저장 순서는 이 정렬 순서와 다를 수 있습니다. UUIDv7은 전역 인과 순서나 연속 순번을 보장하지 않습니다.

#### 메시지 조회와 복구

`GET /rooms/{id}/messages`는 user-service의 방 멤버십과 가입 시각을 확인한 뒤 chat-service를 호출합니다.

| 쿼리 | 동작 |
| :--- | :--- |
| `after_id` 없음 | 최근 이력, 서버는 ID 내림차순 반환 |
| `after_id` 있음 | 해당 ID 이후를 오름차순 반환, 빈 값이면 가입 시각 이후부터 조회 |
| `limit` | 생략 시 이력 100개·동기화 50개, 1000 초과는 1000으로 제한, 응답의 `has_more`로 다음 페이지 판단 |

프론트엔드는 reconnect, `frame_no` 간격, focus, 수동 복구에서 cursor를 2초 되감고 최대 60초 동안 jitter backoff로 반복 조회합니다. 복구 anchor는 사용자·방별 sessionStorage에 보존하며, 같은 복구 구간에서는 실시간 메시지나 빈 응답이 와도 앞으로 옮기지 않습니다. 각 반복은 고정 anchor부터 페이지를 다시 읽어 늦게 저장된 메시지를 포함합니다. 요청당 timeout은 5초이며, 전체 페이지 조회도 복구 window의 남은 시간으로 제한합니다. 별도로 30초마다 최근 cursor를 되감아 조회하고 이때는 전체 페이지 조회에 5초 budget을 적용합니다.

60초가 지나면 anchor 자동 재시도를 멈추고, 마지막 조회 오류가 남아 있으면 재동기화 안내를 표시합니다. 성공 응답에 특정 메시지가 없다는 이유만으로 누락을 판정하지는 않습니다. anchor는 탭 세션 동안 남으며 재연결·focus·수동 복구 때 다시 사용합니다. echo가 10초 안에 오지 않은 송신은 미확인 상태로 표시하고 자동 재전송하지 않습니다.

실패 메시지를 재전송할 때는 기존 `client_msg_id`를 유지합니다. 복구는 MongoDB 저장 지연을 고려한 bounded eventual recovery입니다. 연속 sequence가 없으므로 모든 누락의 존재·복구 완료를 증명하지 않으며, 되감기 범위와 재시도 기간 밖의 지연·시계 차이까지 보장하지 않습니다.

#### 메시지 작성자 표시

메시지는 작성자 ID만 저장하고 username은 저장하지 않습니다. 클라이언트는 방 입장 시 멤버 목록으로 `user_id` → username 캐시를 만들고, 메시지에서 처음 보는 작성자 ID만 `GET /users?ids=...`로 일괄 조회합니다. 현재는 username 변경 기능이 없으므로 주기적 갱신은 두지 않았습니다. 이후 username 변경을 지원하면 캐시 만료나 프로필 변경 이벤트를 별도로 설계해야 합니다.

방을 떠난 사용자의 메시지도 사용자 행이 살아있으면 정상적으로 username을 반환받습니다. 회원탈퇴로 사용자 행이 삭제된 사용자는 일괄 조회 응답에서 제외되며, 클라이언트가 `(탈퇴한 사용자)` 대체 문구로 표시합니다. 메시지 저장 모델이 사용자 프로필에 직접 의존하지 않기 때문에, 작성자 표시 실패가 메시지 조회 자체를 깨뜨리지 않습니다.

### 2.9 설정 관리

애플리케이션 이미지는 환경과 무관하게 동일하게 빌드하고, 실행 환경의 차이는 Kustomize overlay로 주입합니다. 앱 설정의 `ENV`는 `dev`, `test`, `qa`로 나뉩니다. `test` overlay는 E2E의 HTTP 요청과 WebSocket 연결 제한을 완화합니다. 채팅 메시지 제한은 기본값인 초당 2회, 순간 허용 5회를 유지합니다.

설정은 앱이 실제로 읽는 형태를 기준으로 나눕니다.

| 경로 | 담는 값 |
| :--- | :--- |
| ConfigMap `base.yaml` | 포트, 타임아웃, 서비스 주소처럼 공개 가능한 기본값 |
| ConfigMap `override.yaml` | 환경 이름, 관측성 엔드포인트, `test` 처리율 제한 |
| 환경변수 | 인증 Secret, DB 접속 정보, Pod 식별자 |

각 서비스는 ConfigMap 파일을 먼저 읽고, `APP_` 환경변수를 병합한 뒤 설정 구조체를 검증합니다. 공통 설정 타입은 `internal/shared/config`에 두고, 서비스별 설정 트리는 각 서비스 패키지에 둡니다. 필수 값이 비어 있거나 범위를 벗어나면 서비스는 시작되지 않습니다.

Secret YAML은 앱 설정 파일이 아니라 Kubernetes 리소스 정의입니다. Kubernetes는 이 정의로 Secret을 만들고, 컨테이너에는 `envFrom.secretRef`로 `APP_JWT_SECRET`, `APP_DB_POSTGRES_URL` 같은 환경변수를 주입합니다. `POD_NAME`은 Downward API로 주입하며 관측성과 NATS 메시지의 발신 Pod 식별에 사용합니다.

K8s가 자동으로 넣는 Service 환경변수에는 `APP_` prefix가 없으므로 앱 설정을 덮어쓰지 못합니다. 채널 크기와 배치 크기처럼 환경에 따라 바뀌지 않는 내부 한계값은 설정 파일로 빼지 않고 Go 상수로 둡니다.

### 2.10 우아한 종료

HTTP/gRPC 서버는 새 요청을 막고 진행 중인 요청의 종료를 기다립니다. `http.Server.Shutdown`이 WebSocket 연결을 기다리지 않으므로 Manager가 별도로 Hub와 세션을 닫고 NATS 구독을 해제합니다. websocket-service는 MongoDB 저장 큐를 소유하지 않습니다.

chat-service는 종료 시 새 pull을 멈추고 이미 가져온 배치를 제한된 write timeout 안에서 처리합니다. 저장 결과가 확정되지 않아 ACK하지 않은 메시지는 durable consumer에서 다시 전달됩니다. 프로세스가 종료되어도 수락한 backlog는 JetStream에 남습니다. 이 보장은 NATS 데이터가 보존되는 것을 전제로 합니다.

---

## 3. 주요 의사결정

### 3.1 WebSocket 라우팅과 방 구독

Ingress와 Kubernetes Service가 새 연결을 분산합니다. 기존 연결은 해당 Pod에 남고, 같은 방이 여러 Pod에 걸쳐도 각 Pod의 Core NATS 구독이 메시지를 받습니다. queue subscription을 사용하면 한 Pod만 메시지를 받으므로 방 fan-out에는 일반 subscription을 사용합니다.

Pod 증설로 기존 연결을 옮기지 않습니다. 축소·재시작으로 연결이 닫히면 클라이언트가 새 티켓으로 다시 연결하고 MongoDB 이력을 보충합니다. 이 흐름은 [재연결과 메시지 복구](diagrams/seq-reconnect-recovery.mmd)에 정리했습니다.

클라이언트 연결 재시도는 jitter를 포함한 backoff로 연속 실패 기준 최대 20회 수행하고, 연결 성공 시 횟수를 초기화합니다. 티켓 발급의 401·403·429 응답에는 자동 재연결을 중단합니다. 새 Hub는 NATS 구독 flush 후에만 세션을 받습니다. MongoDB 저장과 무관하게 실시간 구독 준비를 확인하기 위한 경계입니다.

### 3.2 장애와 준비 상태

| 상황 | 처리 |
| :--- | :--- |
| NATS disconnect | readiness를 내리고 지연을 분산해 기존 세션 종료, 새 연결은 NATS 재연결과 세션 정리 후 허용 |
| 방 구독 slow consumer 또는 전달 지연 한도 초과 | 해당 방의 로컬 세션 종료 후 reconnect/sync |
| 세션 전송 큐 포화 | 해당 프레임 drop, `frame_no`와 주기적 sync로 복구 시도 |
| MongoDB 장애 | 조회 실패, 저장 worker pull 중단·probe, JetStream 용량 내 새 채팅 수락 |
| JetStream 용량 초과·발행 실패 | 새 채팅 수락 실패, 송신 세션 종료 |
| Pod 종료·축소 | 로컬 세션 종료·구독 해제, 미ACK 저장 작업 재전달 |

#### Liveness와 Readiness

HTTP `/health`와 gRPC TCP liveness는 프로세스 생존을 확인합니다. readiness는 의존성 상태를 반영합니다.

| 서비스 | 준비 상태 검사 |
| :--- | :--- |
| api-gateway `/ready` | Redis `PING`, user gRPC health, `chat.v1.ChatCommand` health |
| websocket-service `/ready` | Redis `PING`, user gRPC health, Manager 동작, NATS 연결 |
| user-service gRPC health | PostgreSQL 상태 |
| chat-service `chat.v1.ChatCommand` | NATS 연결 상태, Kubernetes readiness가 사용 |
| chat-service `chat.v1.ChatQuery` | MongoDB probe 상태 |

`ChatCommand`와 `ChatQuery`는 health service 이름입니다. 별도 송신 RPC는 없으며 사용자 메시지는 WebSocket에서 JetStream으로 발행합니다. MongoDB 장애를 command readiness와 분리해 저장 backlog를 계속 받을 수 있게 합니다. command 준비 상태가 stream 여유 공간이나 다음 publish 성공을 보증하지는 않습니다.

### 3.3 WebSocket 계층 구조

WebSocket Service는 연결 수명, 방 구독, 메시지 발행과 로컬 브로드캐스트를 다룹니다. 이 책임을 한 계층에 모으면 상태 변경 순서를 추적하기 어려워지므로 Router, Manager, Hub, Session으로 나눴습니다. 의존 방향은 위에서 아래로만 흐르게 제한했습니다.

```
Router (1)
└── Manager (1)
    ├── Hub (채팅방 A)
    │   ├── Session (유저 1)
    │   └── Session (유저 2)
    └── Hub (채팅방 B)
        └── Session (유저 3)
```

Manager와 Hub는 액터 모델로 동시성을 처리합니다. Manager는 Hub 목록과 생명주기를 단일 `select` 루프에서 관리하고, Hub는 세션 목록과 브로드캐스트를 자기 루프에서 순서대로 처리합니다. 외부와는 채널 또는 주입된 함수로만 통신해 공유 상태를 직접 잠그는 범위를 줄였습니다. Hub가 종료에 들어가면 atomic flag와 종료 채널로 새 등록·전달을 거절합니다.

#### 계층별 책임

| 계층 | 책임 |
| :--- | :--- |
| Router | HTTP 요청을 검증하고 WebSocket upgrade 전까지의 준비 절차를 조율 |
| Manager | Hub·구독의 생성과 종료, NATS 발행·이벤트 처리를 관리 |
| Hub | 한 방의 로컬 세션과 브로드캐스트를 직렬 처리 |
| Session | 개별 WebSocket 연결의 읽기/쓰기와 메시지 검증을 담당 |

#### 부모 직접 참조 차단

자식 계층이 부모를 직접 참조하면 순환 의존이 생깁니다. 상향 통신이 필요한 경우에도 자식이 부모의 구체 타입을 모르도록 제한합니다.

상향 통신 방식은 호출 성격에 따라 나눴습니다. 부모의 작업 큐로 넘기는 일은 송신 전용 채널로, 호출한 자리에서 바로 결과가 필요한 일은 콜백 함수로 분리했습니다.

| 방식 | 쓰는 기준 | 이 프로젝트의 예 |
| :--- | :--- | :--- |
| 송신 전용 채널 | 자식이 부모의 작업 큐나 이벤트 루프에 일을 맡길 때 | Session이 `unregisterCh`로 종료를 알림 |
| 콜백 함수 | 호출한 자리에서 바로 허용 여부나 처리 결과가 필요할 때 | Session이 메시지 publish 함수와 rate-limit 함수를 호출 |

콜백으로 둔 동작은 부모 액터 루프에서 반드시 직렬화해야 하는 상태 변경이 아닙니다. 메시지 발행 위임이나 처리율 제한 검사처럼 호출한 자리에서 결과를 받아 다음 동작을 정하면 되므로, 채널 요청으로 만들지 않고 함수 주입으로 단순하게 유지했습니다.

### 3.4 JetStream 배치 저장

여러 chat-service Pod가 `chat-persistence` durable pull consumer를 공유해 경쟁 소비합니다. Pod당 기본 worker는 1개이며, worker 하나가 최대 500개 또는 100ms 단위로 가져와 한 배치씩 저장합니다. 기본 write timeout은 5초이고 `AckWait`은 write timeout과 batch wait의 합보다 커야 합니다.

저장 결과는 문서별로 처리합니다.

| 결과 | 처리 |
| :--- | :--- |
| 저장 성공 또는 내용이 같은 중복 | 원본 ACK |
| 일시 오류·결과 불명 | full-jitter delayed NAK, 재전달 |
| 파싱·검증 오류 또는 내용 충돌 | DLQ publish 성공 후 원본 ACK |
| DLQ publish 실패 | 원본 ACK 없이 delayed NAK |

일시 오류는 횟수나 장애 시간만으로 DLQ에 보내지 않습니다. 연속 3회 배치 실패 시 Pod의 circuit을 열어 모든 worker의 새 pull을 멈춥니다. MongoDB probe 실패는 조회 health를 내리지만, 정상 소비 중인 circuit을 직접 열지는 않습니다. Pod당 하나의 probe가 복구를 확인하면 한 worker만 trial batch를 처리하고, 그 배치가 성공해야 정상 소비로 돌아갑니다. 빈 배치만으로 circuit을 닫지 않습니다.

ACK 응답이나 네트워크 문제로 저장된 메시지가 재전달될 수 있어 MongoDB 멱등성 검사가 필요합니다. JetStream의 수락, MongoDB 저장, WebSocket 수신은 서로 다른 완료 시점입니다. file storage와 단일 PVC만으로 노드·디스크 영구 손실까지 견디는 것은 아닙니다.

### 3.5 서비스 간 통신

| 경로 | 방식 |
| :--- | :--- |
| api-gateway → user/chat-service | gRPC 조회·도메인 요청 |
| websocket-service → user-service | gRPC 멤버십 확인 |
| api-gateway → websocket-service | 내부 HTTP 시스템 메시지·방 세션 종료 |
| websocket-service → JetStream | 동기 사용자 채팅 발행 |
| NATS → websocket-service | Core 방 메시지·제어 이벤트 구독 |
| chat-service → JetStream | shared durable pull consumer |

각 gRPC 클라이언트는 `ClientConn`을 재사용하고 Headless Service의 Pod 주소를 `round_robin`으로 분산합니다. 기존 연결이 정상인 동안 새 Pod 발견을 위한 DNS 재조회가 보장되지 않으므로 동적 replica 변경 시 조회 트래픽 분산에는 제한이 있습니다. chat-service의 저장 worker는 gRPC 분산과 별개로 동일 durable consumer에 참여합니다.

### 3.6 bcrypt 워커 풀

bcrypt는 무차별 대입을 어렵게 만들기 위해 의도적으로 느리게 동작하는 CPU 바운드 해시 알고리즘입니다. DB나 네트워크 I/O는 응답을 기다리는 동안 고루틴이 block되어 다른 고루틴이 CPU를 사용할 수 있지만, bcrypt는 실행되는 동안 CPU를 계속 사용합니다. Go 런타임이 동시에 실행할 수 있는 Go 코드는 `GOMAXPROCS` 범위로 제한되므로, CPU 바운드 작업은 많이 시작한다고 처리량이 그만큼 늘지 않습니다.

가입이나 로그인 요청이 몰릴 때 모든 요청 고루틴이 bcrypt를 바로 수행하면, 코어 수보다 많은 해싱 작업이 한정된 CPU 시간을 나눠 갖습니다. 작업 하나가 빨리 끝나기보다 여러 작업이 동시에 조금씩 진행되면서 대기열이 길어집니다. 이 상태가 길어지면 인증 요청뿐 아니라 같은 인스턴스의 다른 요청도 함께 밀립니다.

그래서 bcrypt 연산에는 워커 풀 패턴을 도입했습니다. 워커 수는 `runtime.GOMAXPROCS(0)` 기준으로 두어 동시에 실행되는 해싱 수를 실제 병렬 실행 폭에 맞춥니다. 짧은 순간의 요청 증가는 제한된 큐로 흡수하되, 큐가 가득 차면 더 기다리게 하지 않고 `ErrQueueFull`을 반환합니다.

이 선택은 일부 요청을 빠르게 실패시키는 대신, CPU 경합이 user-service 인스턴스 전체로 번지는 것을 막는 쪽에 가깝습니다. user-service는 `ErrQueueFull`을 `ResourceExhausted`로 바꿔 gateway가 과부하 응답을 낼 수 있게 합니다. 과부하가 런타임의 고루틴 대기열에 숨지 않고, 큐 깊이와 queue full 지표로 관측 가능한 backpressure가 되도록 한 것입니다.

### 3.7 ID 생성 위치와 UUID v7

사용자, 채팅방, 메시지 같은 주요 ID는 DB가 아니라 애플리케이션에서 UUID v7로 생성합니다. ID 생성 시점과 저장 시점을 분리해, 저장 전에 로그와 응답 객체에서 같은 식별자를 사용할 수 있게 하기 위해서입니다.

UUID v7은 생성 시각을 포함하므로 대략적인 시간순 정렬과 로그 추적에 유리합니다. UUID v4보다 B-tree 삽입 위치가 덜 분산되어 인덱스 관리에도 부담이 적습니다. 동시에 auto-increment처럼 전체 레코드 수나 생성 속도를 외부에서 쉽게 추측하게 만들지 않습니다.

애플리케이션에서 ID를 만들면 INSERT 전에 식별자가 확정됩니다. 그래서 관련 엔티티의 FK를 DB 왕복 없이 설정할 수 있고, 비동기 저장 파이프라인에서도 브로드캐스트와 저장을 분리하기 쉽습니다. chat-service는 저장 메시지에 `created_at`이 없으면 UUIDv7의 시각으로 채웁니다.

메시지 정렬과 조회 cursor는 UUIDv7 ID를 사용합니다. 여러 Pod의 생성 시각과 전달 순서가 다를 수 있으므로 연속 순번이나 인과 순서로 해석하지 않습니다.

---

## 4. Kubernetes 배포 설계

K8s 전환의 목표는 서비스 인스턴스 수가 동적으로 변해도 서비스 동작과 데이터 정합성이 유지되는지 검증하는 것입니다. Kubernetes에서는 서비스 인스턴스가 Pod 단위로 생성·종료되고 트래픽 대상에서 제외됩니다. 애플리케이션은 readiness와 종료 절차로 트래픽 수신 가능 상태를 명확히 드러내야 합니다.

리소스 구분은 다음과 같습니다.

| K8s 리소스 | 역할 | 선택 이유 |
| :--- | :--- | :--- |
| Deployment | 앱과 데이터 서비스 | replica 수 조절과 rollout 대상 |
| StatefulSet + PVC | NATS JetStream | Pod 재시작 후 같은 file store 재사용 |
| Service | 안정적인 내부 진입점 | 파드 IP 변경을 DNS 이름 뒤로 숨김 |
| Headless Service | gRPC 대상 발견 | 고정 replica에서 Pod 목록을 직접 보고 `round_robin` 수행 |
| Job | 일회성 마이그레이션 | 성공/실패와 완료 상태가 명확함 |
| Ingress | 외부 HTTP/WebSocket 진입점 | 외부 경로를 서비스로 라우팅 |

K8s 안에서 각 워크로드와 데이터 계층이 배치되는 구조는 [Kubernetes 실행 아키텍처](diagrams/flow-k8s-architecture.mmd)에 정리했습니다.

### 4.1 Manifest 구조

K8s manifest는 `base`와 overlay로 나눕니다. `base`에는 서비스 구조와 probe처럼 공통인 리소스를 두고, overlay에는 replica 수와 리소스 정책처럼 환경별 차이를 둡니다.

| Overlay | 목적 | 특징 |
| :--- | :--- | :--- |
| `dev` | 로컬 개발과 C10K 부하 확인 | 앱은 대부분 1 replica, `websocket-service`는 C10K 기준에 맞춰 2 replicas |
| `test` | 자동화된 K8s 전체 시나리오 검증 | 주요 gateway/service `replicas: 2`, 메모리 request/limit으로 테스트 격리 |
| `qa` | WebSocket HPA 확장·축소 중 전달과 복구 검증 | WebSocket HPA `1→2` 정합성 검증 |

`local` 대신 `dev/test/qa`로 나눈 이유는 실행 위치가 아니라 검증 목적을 드러내기 위해서입니다. 각 overlay의 차이는 표처럼 실행 목적에 맞춰 제한합니다.

### 4.2 Phase Overlay와 Bootstrap 스크립트

로컬 K8s 환경의 부트스트랩 순서는 [bootstrap.sh](../deploy/k8s/scripts/bootstrap.sh)가 제어합니다. 스크립트는 `K8S_ENV`에 맞는 overlay를 선택하고, phase별 Kustomize overlay를 순서대로 적용한 뒤 각 단계의 준비 상태를 기다립니다. manifest를 한 번에 적용하면 DB 준비나 migration 완료 전에 앱이 뜰 수 있으므로, 데이터 계층, migration, 앱 rollout을 명시적으로 직렬화했습니다.

| Phase | 스크립트 동작 | 완료 조건 |
| :--- | :--- | :--- |
| `foundation` | Secret과 Postgres/Mongo/Redis/NATS overlay 적용 | Deployment와 NATS StatefulSet 준비 완료 |
| `observability` | Grafana 스택 ConfigMap 생성 후 observability overlay 적용 | 관측성 Deployment rollout 완료 |
| `migrations` | migration ConfigMap 생성, 기존 Job 삭제, migration overlay 적용 | `postgres-migrate`, `mongo-migrate` Job 완료 |
| `apps` | OpenAPI ConfigMap 생성, apps overlay 적용, 핵심 백엔드 → WebSocket Service → 진입 계층 순서로 rollout restart | 앱 Deployment rollout 완료 |

마이그레이션이 실패하면 앱 rollout을 진행하지 않습니다. 스키마가 불확실한 상태에서 앱을 띄우지 않기 위한 즉시 중단 전략입니다.

부하 검증은 [load.sh](../deploy/k8s/scripts/load.sh)가 별도로 실행합니다. 환경을 띄우는 일과 부하를 거는 일을 분리해야 실패 원인을 좁히기 쉽기 때문입니다. `load.sh`는 k6 ConfigMap을 만들고 기존 Job을 삭제한 뒤 load overlay를 적용합니다. QA에서는 HPA 테스트 전 `websocket-service`를 1 replica로 되돌리고 HPA를 다시 붙여 스케일아웃을 매번 같은 시작 상태에서 검증합니다.

### 4.3 Runtime 리소스 모델

계속 요청을 처리하는 서비스는 Deployment로 둡니다.

| Deployment | 역할 | Service |
| :--- | :--- | :--- |
| `api-gateway` | REST API와 WebSocket 티켓 발급 | ClusterIP |
| `websocket-service` | WebSocket 세션과 로컬 방 구독 | ClusterIP |
| `user-service` | 사용자/방/멤버십 gRPC 서비스 | Headless |
| `chat-service` | JetStream 저장 worker와 gRPC 조회 | Headless |
| `frontend` | 정적 프론트엔드 | ClusterIP |
| `swagger-ui` | OpenAPI 문서 UI | ClusterIP |

`api-gateway`, `websocket-service`, `frontend`, `swagger-ui`는 ClusterIP Service를 사용합니다. WebSocket은 연결을 수락한 Pod에서 유지되며 방별 고정 라우팅이나 sticky session을 요구하지 않습니다.

`user-service`와 `chat-service`는 Headless Service를 사용합니다. gRPC 클라이언트가 대상 Pod IP 목록을 보고 `round_robin`으로 RPC를 분산합니다.

base manifest에는 resource request/limit을 넣지 않습니다. 리소스 정책은 실행 목적에 따라 달라지므로 overlay에서만 추가합니다.

`dev`는 Compose 시절 C10K 기준과 비교할 수 있게 앱 메모리 limit을 두지 않습니다. `websocket-service`는 2 replicas로 시작하고, `user-service`만 bcrypt 워커 풀 검증용 CPU limit 4를 둡니다. TCP accept queue 관련 sysctl은 주요 서비스에 적용합니다.

`test`는 성능 기준이 아니라 반복 가능한 전체 시나리오 검증 환경입니다. gateway/service 계열은 2 replicas로 고정하고, 로컬 머신 전체 메모리를 잠식하지 않도록 memory request/limit을 둡니다.

`qa`는 `websocket-service`에 활성 연결 수 기준 HPA를 적용합니다. 기본 범위는 1~2 replicas, Pod당 목표는 100 connections입니다. 확장 시 기존 연결이 유지되고, 축소 시 끊긴 연결이 새 Pod에서 복구되는지 확인합니다. 이미 열린 연결은 새 Pod로 자동 재분배되지 않습니다.

user-service와 chat-service의 HPA는 구성하지 않았습니다. chat-service worker replica 변경은 같은 consumer에 대한 경쟁 소비로 동작하며, 조회 RPC의 동적 분산은 별도 문제입니다.

### 4.4 Local Data Layer

로컬 실행과 장애 복구 검증을 위한 데이터 계층입니다.

| 리소스 | 구현 | 저장소 | 현재 이미지 |
| :--- | :--- | :--- | :--- |
| PostgreSQL | Deployment + ClusterIP | `emptyDir` | `postgres:17` |
| MongoDB | Deployment, `Recreate` + ClusterIP | `mongo-data` PVC 8Gi, RWO | `mongo:7.0` |
| Redis | Deployment + ClusterIP | 메모리 | `redis:7-alpine` |
| NATS | StatefulSet 1 replica + ClusterIP | `data` PVC 2Gi, RWO | `nats:2.14.6-alpine` |

NATS는 `/data/jetstream`에 file store를 두며 서버의 최대 file store는 1536MB입니다. client `4222`, monitor `8222`, exporter `7777` 포트를 사용합니다. exporter 이미지는 `natsio/prometheus-nats-exporter:0.20.1`입니다.

MongoDB와 NATS는 같은 PVC를 유지한 Pod 재시작을 검증합니다. PostgreSQL과 Redis까지 영속화한 운영 배포는 아니며, PVC 삭제·노드 또는 볼륨 영구 손실에 대한 복제와 백업은 구성하지 않았습니다.

### 4.5 Ingress와 외부 경로

Ingress는 브라우저와 테스트 클라이언트가 접근하는 외부 경로를 하나로 모읍니다.

| 경로 | 대상 | 목적 |
| :--- | :--- | :--- |
| `/` | `frontend` | 프론트엔드 |
| `/api` | `api-gateway` | REST API |
| `/api/auth/ws-ticket` | `api-gateway` | 일회성 WebSocket 티켓 발급 |
| `/ws` | `websocket-service` | WebSocket upgrade |
| `/docs` | `swagger-ui` | OpenAPI 문서 UI |
| `/grafana` | `grafana` | 로컬 관측 대시보드 |

같은 kind 클러스터에 `dev`, `test`, `qa` namespace를 동시에 띄울 수 있으므로, Ingress host는 overlay에서 분리합니다. namespace와 host를 함께 나눠 외부 라우팅 규칙 충돌을 피합니다.

kind 로컬 클러스터에서는 host port가 control-plane node에 매핑됩니다. ingress-nginx controller를 해당 node에 고정해, 로컬 브라우저와 E2E runner가 같은 Ingress 경로를 사용하게 합니다.

프론트엔드와 API는 refresh token 쿠키를 same-origin으로 다루기 위해 같은 host 아래에 둡니다. access token은 `Authorization` 헤더, refresh token은 HttpOnly cookie로 전달합니다. 이 구조에서는 Ingress 경로 라우팅만으로 CORS와 쿠키 경로 처리를 단순하게 유지할 수 있습니다.

WebSocket 경로는 일반 HTTP와 달리 upgrade 연결이 길게 유지됩니다. 짧은 기본 timeout 때문에 정상 채팅 연결이 끊기지 않도록 Ingress에 WebSocket timeout 관련 nginx annotation을 명시합니다.

### 4.6 Probe와 Lifecycle

K8s에서 probe는 단순 헬스체크가 아니라 rollout과 트래픽 라우팅의 기준입니다. 특히 readiness와 liveness를 섞으면 장애 대응이 오히려 나빠질 수 있습니다.

| Probe | 의미 | 이 프로젝트의 기준 |
| :--- | :--- | :--- |
| startupProbe | 느린 초기화를 기다림 | 앱 부팅 중 liveness 오판 방지 |
| livenessProbe | 프로세스가 살아있는지 확인 | HTTP는 `/health`, gRPC는 TCP socket |
| readinessProbe | 트래픽을 받아도 되는지 확인 | HTTP는 `/ready`, gRPC는 native gRPC health |

`tcpSocket`과 `grpc`는 Kubernetes가 제공하는 probe 방식입니다. `tcpSocket`은 지정한 포트에 연결이 열리는지만 확인하고, `grpc`는 애플리케이션이 등록한 gRPC Health Checking Protocol의 `Check` 응답을 확인합니다.

user-service readiness는 PostgreSQL 상태를 반영합니다. chat-service readiness는 `chat.v1.ChatCommand`를 검사하며 NATS 연결에 의존합니다. MongoDB 상태는 `chat.v1.ChatQuery`로 따로 노출해 DB 장애가 메시지 수락 경로 전체를 차단하지 않게 합니다. 실제 조회 요청은 DB 장애 시 실패할 수 있습니다.

WebSocket의 열린 세션은 Manager가 직접 닫습니다. chat-service는 새 pull을 멈추고 진행 중인 배치를 처리하며, 미ACK 메시지는 JetStream에서 재전달됩니다. Deployment의 `terminationGracePeriodSeconds`는 이 종료 절차를 위한 시간을 제공합니다.

### 4.7 E2E 실행 기준

Docker Compose 실행 경로는 기본 실행 경로에서 제거했습니다. 현재 E2E는 K8s `test` overlay가 이미 bootstrap되어 있다는 전제로 실행됩니다.

```bash
make test-up
go test -count=1 -tags=e2e ./test/e2e
```

통합 테스트까지 함께 확인할 때는 같은 `go test` 명령에 build tag를 같이 넘깁니다.

```bash
go test -count=1 -tags=integration,e2e ./...
```

기본 엔드포인트는 다음과 같습니다. 클러스터 밖에서 실행되는 Go E2E runner가 Ingress를 통해 실제 HTTP/WebSocket 경로를 검증합니다.

| 환경변수 | 기본값 | 용도 |
| :--- | :--- | :--- |
| `E2E_GATEWAY_BASE_URL` | `http://test.gochat.localhost:30080/api` | REST API |
| `E2E_WS_BASE_URL` | `http://test.gochat.localhost:30080` | WebSocket `/ws`의 base URL |
| `E2E_K8S_NAMESPACE` | `go-chat-test` | readiness/replica/장애 복구 검증 대상 |

`test` overlay는 주요 gateway/service를 `replicas: 2`로 고정합니다. HPA를 바로 붙이면 replica 변화와 부하 변화가 섞여 실패 원인을 좁히기 어렵습니다. 고정 replica에서 정합성을 확인한 뒤, `qa` overlay에서 WebSocket HPA 조건을 검증합니다.

E2E는 PostgreSQL 데이터를 truncate하고 MongoDB 문서를 `deleteMany`로 지워 인덱스를 유지합니다. Redis는 인증·티켓·처리율 제한 등 테스트가 만든 키만 지웁니다. NATS StatefulSet의 준비 상태도 확인하며, 저장 장애 시나리오는 MongoDB 중단·복구, chat-service 재시작·replica 변경, 같은 PVC를 사용하는 NATS 재시작 후 수락 메시지의 최종 저장을 확인합니다.

### 4.8 K6 부하와 HPA 정합성 검증

부하 테스트는 목적별로 분리합니다.

| 명령 | 대상 | 시나리오 | 목적 |
| :--- | :--- | :--- | :--- |
| `make dev-load` | `go-chat-dev` | `c10k-test.js`, k6 Pod 4개 | JetStream 수락·실시간 전달 성능 측정 |
| `make qa-load` | `go-chat-qa` | `hpa-test.js`, k6 Job 1개 | HPA 확장과 재연결 중 전달·복구 검증 |

`dev-load`는 많은 연결과 메시지를 넣는 성능/부하 시나리오입니다. k6 자체가 병목이 되지 않도록 Kubernetes Job `parallelism=4`, `completionMode=Indexed`로 k6 Pod 4개를 띄우고, 각 Pod가 VU offset을 나눠 가집니다.

두 부하 시나리오는 클러스터 안에서 `http://api-gateway:8080`, `ws://websocket-service:8081`로 직접 요청합니다. Ingress를 경유하는 브라우저·E2E와 달리 Ingress 구간의 지연과 처리량은 측정하지 않습니다.

`qa-load`는 `websocket-service`를 1 replica로 되돌린 뒤 HPA를 붙여 확장과 재연결을 검증합니다. 활성 연결 중 2→1 강제 축소는 [HPA 보고서의 추가 절차](K8S_JETSTREAM_HPA_REPORT.md#연결-유지-중-websocket-scale-in)로 수행했습니다. echo로 확인한 ID와 DB 반영, 중복·복구 오류를 대조하며, 종료 뒤 pending·DLQ 등 별도 관측 결과도 보고서에 기록합니다. 부하·정합성 테스트는 요청 또는 승인된 실험 계획이 있을 때 실행합니다.

### 4.9 Docker Compose 제거와 기준점 보존

Docker Compose는 더 이상 기본 실행 경로가 아닙니다. 개발 확인과 E2E 모두 K8s 기준으로 정리했습니다. Dockerfile은 계속 필요합니다. 로컬 이미지를 빌드하고 kind/OrbStack/K8s 클러스터에 이미지를 제공하기 위해서입니다.

Compose 제거 전에 `legacy-compose-baseline` annotated tag를 남겼습니다. 나중에 “Compose 대비 K8s 성능 지표”를 비교하고 싶을 때, 기본 브랜치에 Compose 파일을 계속 들고 갈 필요 없이 해당 tag를 기준점으로 참조할 수 있습니다.

Compose 제거의 효과는 문서와 E2E가 K8s 하나만 바라보게 하는 것입니다. “Compose에서는 되는데 K8s에서는 안 되는” 이중 실행 경로를 줄이는 대신, 처음 실행할 때는 K8s bootstrap이 먼저 필요합니다. README는 dev/test/qa 실행 명령의 기준 문서로 두고, 디자인 문서는 구조의 판단 근거를 설명합니다.

---

## 5. 테스트 전략

테스트 전략은 빠른 로직 검증과 실제 실행 경로 검증을 분리합니다. 단위 테스트는 외부 의존성을 끊고, 통합 테스트는 실제 저장소와의 차이를 확인하며, E2E 테스트는 K8s `test` overlay에서 사용자 흐름을 검증합니다.

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
- 테이블 기반 서브테스트(`t.Run`)로 정의하고 `t.Parallel()` 병렬 실행을 기본으로 한다.
- 서브테스트 이름은 `Success: 설명` / `Failure: 설명 (에러코드)` 형식으로 맞춘다.

#### 통합 테스트

- Testcontainers로 실제 PostgreSQL/MongoDB/Redis/NATS를 띄우고 시나리오 위주로 검증한다.
- JetStream 수락·재전달, MongoDB 중복·부분 실패, Redis 원자 처리처럼 실제 서버 동작에 의존하는 경계를 검증한다.
- 데이터 오염을 막기 위해 순차 실행하고, 매 테스트마다 데이터를 초기화한다.

#### E2E 테스트

- K8s `test` overlay로 전체 시스템을 띄우고 블랙박스로 검증한다.
- E2E suite는 이미 bootstrap된 `go-chat-test` namespace의 readiness를 확인한 뒤 실행한다.
- 테스트 간 데이터 정리는 K8s 내부 PostgreSQL truncate와 MongoDB 문서 삭제로 수행한다.
- 장애 테스트는 수락 메시지 ID와 최종 DB 집합을 비교하고 pending·DLQ를 함께 확인한다.
- 시나리오 번호 순서대로 사용자 여정을 이어가며 검증한다.

---

## 6. 관측성

관측성은 장애 위치를 빠르게 좁히기 위한 흐름으로 구성합니다. 메트릭으로 이상 범위를 잡고, 트레이스와 로그로 요청 맥락을 확인하며, 프로파일로 코드 병목을 봅니다.

메트릭과 트레이스는 앱의 OpenTelemetry SDK가 OTLP/HTTP로 Alloy에 전송합니다. 로그는 앱이 `slog`로 표준 출력에 기록하고, Alloy의 `loki.source.kubernetes`가 Kubernetes API를 통해 Pod 로그 스트림을 읽습니다. Alloy는 로그를 Loki Push API로, 메트릭을 Prometheus Remote Write로, 트레이스를 Tempo OTLP/HTTP로 전송합니다. 프로파일은 앱의 `pyroscope-go`가 Pyroscope로 직접 전송합니다.

### 6.1 신호 구성

| 신호 | 백엔드 | 용도 |
| :--- | :--- | :--- |
| 로그 | Loki | 이벤트 기록 검색 |
| 메트릭 | Prometheus | 이상 감지 |
| 트레이스 | Tempo | 서비스 간 요청 흐름 추적 |
| 프로파일 | Pyroscope | 코드 레벨 병목 분석 |

### 6.2 계측 기준

| 기준 | 적용 |
| :--- | :--- |
| 상관관계 | HTTP/gRPC 요청 경계에서 trace context 전파 |
| 노이즈 제거 | `/health`, `/ready`, 메트릭 엔드포인트는 로그/트레이스 수집에서 제외 |
| 민감정보 보호 | 토큰/비밀번호/시크릿 쿼리 파라미터 마스킹, DB 쿼리문 노출 통제 |
| 카디널리티 제어 | URL path의 UUID를 `:id`로 정규화 |
| 커버리지 | HTTP/gRPC, 저장소, WebSocket/NATS, JetStream persistence, 인증 경로 계측 |
| 샘플링 | 트레이스는 10% 샘플링 |

수집량도 설계 대상입니다. `/health`, `/ready` 같은 probe 요청은 장애 분석보다 노이즈를 많이 만들기 때문에 로그와 트레이스에서 제외합니다. URL path에 UUID를 그대로 남기면 사용자나 방마다 메트릭 시계열이 늘어나므로 `:id`로 정규화합니다. 트레이스는 10%만 샘플링해 로컬 검증 환경의 비용을 제한합니다.

### 6.3 통합 조회

Grafana 대시보드는 장애 분석 흐름에 맞춥니다. 전체 상태에서 시작해 API traffic, realtime, 저장소, platform runtime 지표로 범위를 좁힙니다.

| 대시보드 | 내용 |
| :--- | :--- |
| Operations Overview | active alerts, availability, latency, traffic, realtime, persistence, platform 핵심 상태 |
| API Traffic | HTTP/gRPC 요청률, latency, error, route/method별 상위 지표 |
| Realtime Messaging | WebSocket 연결, message RPS, drop/rate limit, NATS, UUID 역전, fan-out |
| Data Persistence | PostgreSQL, MongoDB, Redis pool 및 저장 지표 |
| Platform Runtime | Kubernetes replica/restart/HPA, container CPU/memory, Go runtime |

NATS 메시지 경계에서는 트레이스 컨텍스트를 전파하지 않습니다. 메시지 전달과 저장 경로는 전용 메트릭과 프로파일로 확인합니다. 저장 대기량·지연·재시도·DLQ와 Pod별 회로 차단 상태는 [텔레메트리 카탈로그](TELEMETRY_CATALOG.md#persistence)에 정리합니다.

`trace_id`를 기준으로 Loki 로그와 Tempo 트레이스를 연결합니다. 로그/트레이스/프로파일은 별도 커스텀 대시보드보다 Grafana Explore와 drilldown 화면을 기본 경로로 사용해 장애 분석 중 화면 전환 비용을 줄입니다.

---

## 7. 검증 범위와 운영 경계

JetStream 구성의 검증 결과는 [JetStream C10K 보고서](K8S_JETSTREAM_C10K_REPORT.md)와 [JetStream HPA·장애 복구 보고서](K8S_JETSTREAM_HPA_REPORT.md)에 기록되어 있습니다. 기존 [Kubernetes C10K 보고서](K8S_C10K_REPORT.md)와 Compose 보고서는 당시 구조의 기준 기록입니다. 실행 조건이 달라 수치 차이를 JetStream만의 비용으로 해석하지 않습니다.

2026-09-16 C10K 실행의 최대 worker P99는 50.00ms로 사전 기준 `<50ms`를 충족하지 못했습니다.

### 7.1 검증 범위

| 구분 | 기준 |
| :--- | :--- |
| 실행 | 로컬 kind, `dev`/`test`/`qa` overlay |
| E2E | `test` overlay의 다중 Pod 사용자 흐름과 저장 장애 복구 |
| HPA | `qa` WebSocket 확장·축소, 기존 연결 유지와 재연결 |
| 영속성 | MongoDB 복구, worker 변경, 같은 PVC의 NATS 재시작 |
| 프론트엔드 | UUIDv7 정렬, 메시지 키별 중복 제거, 제한된 기간의 복구 |

장애 검증은 보고서의 메시지 집합과 실행 시나리오에 한정됩니다. 장시간 장애, 임의의 시계 차이, 모든 실시간 프레임의 누락 검출, 디스크 영구 손실에 대한 무손실 보장은 포함하지 않습니다.

### 7.2 보안 경계

`local-secret.yaml`은 로컬 dev/test/qa용 샘플 값만 담습니다. 실제 인증 정보는 Git에 올리지 않고 외부 Secret 저장소나 배포 파이프라인에서 주입해야 합니다.

현재 설계는 로컬에서 재현 가능한 실행과 검증에 초점을 둡니다. 실제 운영으로 확장하려면 NATS 인증·TLS·NetworkPolicy, 복제·백업과 PVC 복구, 멀티 노드 장애, 롤링 업데이트 중 연결 정책을 별도로 설계해야 합니다.
