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

이 프로젝트의 설계 목표는 채팅방 메시지를 빠르게 전달하면서도 메시지 정합성과 수평 확장성을 함께 만족하는 실시간 채팅 MSA를 만드는 것입니다.

MSA로 구성한 이유는 사용자/방 관리, 메시지 저장, 실시간 연결의 부하 특성과 상태 관리 방식이 다르기 때문입니다. 외부 클라이언트에는 REST API를 제공하고, 내부 서비스 간 통신에는 gRPC를 사용하며, 실시간 메시지는 WebSocket으로 주고받습니다. 이렇게 나누면 부하가 몰리는 서비스만 선택적으로 확장할 수 있습니다. 예를 들어 메시지 송수신 부하가 커지면 WebSocket Service 인스턴스만 늘리면 되고, 인스턴스 하나가 중단되어도 나머지 인스턴스가 새 연결과 재접속 요청을 처리할 수 있습니다.

채팅 메시지를 빠르게 전달하기 위해 방 단위 브로드캐스트는 외부 메시지 브로커를 거치지 않고 WebSocket Service 인스턴스의 메모리 안에서 처리합니다. 이를 위해 같은 채팅방의 연결을 하나의 인스턴스로 라우팅합니다. 이 구조에서는 방마다 메시지 순서를 한 곳에서 부여할 수 있어 정합성 관리도 단순해집니다. 인스턴스 교체 중에는 기존 인스턴스가 이미 받은 메시지의 순번 부여와 저장 처리를 마무리한 뒤 새 인스턴스가 방 소유권을 이어받습니다. 새 인스턴스는 기존 인스턴스가 마지막으로 발급한 순번 이후부터 메시지를 처리하므로, 이미 사용된 순번이 다시 발급되는 충돌을 막습니다.

저장소는 데이터 성격에 따라 나눕니다. 사용자, 채팅방, 멤버십처럼 관계와 무결성이 중요한 데이터는 PostgreSQL에 저장합니다. 채팅 메시지는 본문과 메타데이터를 한 문서로 저장하고, 메시지 형태가 늘어나도 스키마 변경 부담이 작도록 MongoDB에 저장합니다. Redis는 영속 데이터 저장소가 아니라 인증 토큰이나 WebSocket 연결 티켓처럼 만료 정책이 있고 자주 갱신되는 제어 상태를 관리합니다.

실행과 검증은 로컬 kind 환경에서 가능합니다. 개발 환경에서는 기능 확인과 부하 테스트를 수행합니다. 테스트 환경에서는 멀티 인스턴스 기준으로 전체 시나리오를 검증합니다. 품질 검증 환경에서는 수평 확장 중 메시지 정합성을 확인합니다.

### 1.2 서비스 구성

아래 표는 서비스별 책임, 통신 방식, 주요 상태를 정리한 것입니다.

| 서비스 | 책임 | 통신 | 상태/저장소 |
| :--- | :--- | :--- | :--- |
| api-gateway | REST API 진입점, JWT 검증 | HTTP | Redis (요청 제한) |
| ws-gateway | WebSocket 티켓 발급, 방 기준 인스턴스 선택, WebSocket 프록시 | HTTP/WebSocket | Redis (티켓, 요청 제한, WebSocket Service 인스턴스 목록) |
| websocket-service | 세션 관리, 방 단위 브로드캐스트, 메시지 순번과 방 소유권 관리 | WebSocket | Redis (방 소유권, 마지막 순번 기준) |
| user-service | 사용자, 채팅방, 멤버십, refresh token 관리 | gRPC | PostgreSQL, Redis (refresh token 상태) |
| chat-service | 메시지 저장, 이력 조회, 누락 메시지 조회 | gRPC | MongoDB |

서비스 경계와 저장소 의존성은 [MSA 앱 아키텍처](diagrams/flow-msa.mmd)에 정리했습니다.

### 1.3 주요 기술 선택

아래 표는 주요 기술 선택과 그 근거를 정리한 것입니다.

| 영역 | 선택 | 근거 |
| :--- | :--- | :--- |
| 언어/런타임 | Go 1.26 | 연결별 read/write loop를 단순하게 구성하고, 정적 바이너리로 배포를 단순화하기 위함 |
| 외부 API | `net/http`, OpenAPI | 프레임워크 의존을 줄이고, OpenAPI 명세로 요청/응답 계약을 먼저 고정하기 위함 |
| 내부 통신 | gRPC, Buf | 서비스 간 계약을 `.proto`로 명확히 정의하고, 생성 코드로 호출부 불일치를 줄이기 위함 |
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
| 경로 | 동작이 아니라 리소스를 표현합니다. 예: `POST /rooms`, `GET /rooms/{id}` |
| 필드명 | 쿼리 파라미터와 JSON 필드는 `snake_case`로 통일합니다. 예: `room_id`, `created_at` |
| 에러 | 오류 응답은 HTTP API 표준 형식인 Problem Details(RFC 9457)를 사용하며, `type`, `title`, `status`, `detail`을 포함합니다. |
| 처리율 제한 | 요청이 허용량을 넘으면 HTTP 429 Too Many Requests를 반환합니다. 응답의 `Retry-After` 헤더로 클라이언트가 언제 다시 시도할 수 있는지 알려줍니다. |

#### 내부 gRPC

내부 gRPC는 [Google API Design Guide](https://cloud.google.com/apis/design)를 기준으로 설계합니다. 서비스 간 호출 계약은 [api/proto/](../api/proto/)의 `.proto` 파일에 먼저 정의하고, 생성된 Go 코드로 서버와 클라이언트 타입을 맞춥니다.

| 항목 | 기준 |
| :--- | :--- |
| 계약 | 요청/응답 메시지와 서비스 메서드를 `.proto`에 정의합니다. 예: `BatchGetUsersRequest`, `UserService.BatchGetUsers` |
| 메서드 | 서비스 간 호출 의도가 드러나도록 동사 중심으로 이름을 붙입니다. 예: `BatchGetUsers`, `ListMessages`, `GetLastSequenceNumber` |
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

브라우저에는 refresh token을 `HttpOnly`, `SameSite=Strict` 쿠키로 전달합니다. `HttpOnly`는 자바스크립트에서 토큰을 읽지 못하게 해 XSS 피해를 줄이고, `SameSite=Strict`는 다른 사이트에서 시작된 요청에 쿠키가 자동으로 실리는 상황을 막아 CSRF 위험을 줄입니다. 현재 프론트엔드는 Nginx 리버스 프록시를 통해 API를 호출하므로 same-origin 경로에서 인증 쿠키를 다룹니다. 운영 환경에서는 HTTPS에서만 쿠키가 전송되도록 `Secure` 속성을 추가해야 합니다.

Redis 장애로 refresh token 상태를 확인하거나 갱신할 수 없으면 로그인, 토큰 재발급, 로그아웃 요청은 실패시킵니다. 유효성을 확인할 수 없는 토큰을 허용하지 않기 위해서입니다.

로그인부터 WebSocket 티켓 발급까지의 흐름은 [로그인과 WebSocket 인증 시퀀스](diagrams/seq-auth-ticket.mmd)에 정리했습니다. refresh token 회전 흐름은 [refresh token rotation 시퀀스](diagrams/seq-refresh-token-rotation.mmd)에 따로 정리했습니다.

#### WebSocket 티켓

브라우저 WebSocket API는 `Authorization` 헤더를 직접 지정할 수 없습니다. 그래서 연결 요청에는 URL 쿼리 파라미터를 써야 하는데, JWT를 그대로 넣으면 서버 로그나 브라우저 히스토리에 토큰이 남을 수 있습니다.

이를 피하기 위해 WebSocket 연결 전에 30초 TTL의 일회성 티켓을 발급합니다. 티켓은 UUID 기반 opaque token이며, Redis의 `ws:ticket:{uuid}` key에 저장됩니다. 연결 시에는 이 티켓을 원자적으로 소비하므로 같은 티켓을 다시 사용할 수 없습니다.

ws-gateway 인스턴스가 여러 개여도 같은 Redis를 공유하므로, 티켓 발급과 사용 여부는 한 곳에서 판단됩니다.

#### 내부 통신 시크릿

ws-gateway는 공개 엔드포인트(`/ws/ticket`, `/ws`)와 내부 엔드포인트(`/internal/*`)를 같은 포트에서 제공합니다. 네트워크 레벨에서 완전히 분리된 구조가 아니므로, api-gateway가 ws-gateway의 내부 API를 호출할 때 `X-Internal-Secret` 헤더로 호출 주체를 확인합니다. 시크릿은 환경설정으로 주입하는 사전 공유 키입니다.

시크릿 비교는 `crypto/subtle.ConstantTimeCompare`로 처리해 문자열 비교 시간 차이가 노출되지 않게 합니다.

현재 api-gateway와 ws-gateway 두 서비스만 정적 시크릿을 공유합니다. 이 값은 로컬 dev/test/qa Secret에서 주입하며, 실제 인증 정보는 Git에 두지 않습니다.

### 2.3 처리율 제한 전략

처리율 제한은 요청 성격에 따라 기준을 다르게 둡니다. 공개 API와 WebSocket 연결 요청은 클라이언트 IP로 제한하고, 인증 API와 WebSocket 티켓 발급은 사용자 ID 기준으로 제한합니다. 채팅 메시지는 같은 사용자가 같은 방에 과도하게 보내는 경우를 막습니다.

HTTP 요청은 허용량을 넘으면 HTTP 429 Too Many Requests로 거부하고, `Retry-After` 헤더로 재시도 시점을 알려줍니다. WebSocket 메시지는 연결을 바로 끊지 않고 해당 세션에 제한 경고를 보냅니다. 내부 통신 경로(`/internal/*`)는 외부 사용자가 직접 호출하는 경로가 아니므로 제한 대상에서 제외합니다.

#### HTTP 요청 제한 (api-gateway, ws-gateway)

| 대상 | 제한 기준 | 방어 목적 | 기본값 |
| :--- | :--- | :--- | :--- |
| api-gateway 공개 API | 클라이언트 IP | 로그인 시도와 공개 API 남용 방어 | 초당 5회 / 순간 허용 10회 |
| api-gateway 인증 API | 사용자 ID | 로그인 후 API 과다 호출 방어 | 초당 10회 / 순간 허용 20회 |
| ws-gateway WebSocket 연결 | 클라이언트 IP | WebSocket 연결 요청 남용 방어 | 초당 5회 / 순간 허용 10회 |
| ws-gateway 티켓 발급 | 사용자 ID | 연결 시도 폭주 방어 | 초당 2회 / 순간 허용 5회 |

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

MongoDB는 채팅 메시지를 `messages` 컬렉션에 저장합니다. 메시지는 사용자/방 데이터처럼 관계를 계속 갱신하는 데이터가 아니라, 생성된 뒤 시간순 조회와 재동기화에 사용되는 추가 중심 데이터입니다.

MongoDB를 선택한 핵심 이유는 유연한 스키마와 부하 분리입니다. 일반 메시지, 시스템 메시지, 첨부 파일처럼 메시지 형태가 늘어날 수 있고, 메시지 저장/조회 부하를 사용자/방 트랜잭션과 분리할 수 있습니다. 중복 전송과 순번 충돌은 MongoDB의 유니크 인덱스로 최종 방어합니다.

| 인덱스 | 종류 | 용도 |
| :--- | :--- | :--- |
| `{ roomId, clientMsgId }` | UNIQUE | 클라이언트 메시지 중복 방지 |
| `{ roomId, sequenceNumber }` | UNIQUE | 방 안에서 같은 순번이 중복 저장되는 상황 방지 |
| `{ createdAt }` | TTL (90일) | 오래된 메시지 자동 파기 |

TTL 90일은 메시지 보관 기간을 제한해 저장 비용을 통제하기 위한 값입니다.

#### Redis (제어 상태)

Redis에는 시간이 지나면 사라지거나 빠르게 바뀌는 제어 상태만 둡니다. 영속 도메인 데이터는 PostgreSQL/MongoDB에 남기고, Redis는 인증 상태, 일회성 티켓, 처리율 제한, WebSocket 라우팅 상태를 관리합니다.

| 키 | 값 | 책임 서비스 | 만료/갱신 | 용도 |
| :--- | :--- | :--- | :--- | :--- |
| `auth:rt:active:{tokenHash}` | 사용자 ID | user-service | 토큰 만료 시간 | 현재 유효한 Refresh Token |
| `auth:rt:used:{tokenHash}` | 사용자 ID | user-service | 이전 토큰의 남은 만료 시간 | 이미 회전된 Refresh Token 표시, 재사용 탐지 |
| `auth:rt:user:{userID}` | 유효 토큰 해시 목록 | user-service | 토큰 만료 시간으로 갱신 | 사용자 단위 전체 폐기 |
| `ws:ticket:{uuid}` | 사용자 ID | ws-gateway | 30초, 사용 시 삭제 | WebSocket 연결용 일회성 티켓 |
| `rate:*` | 처리율 제한 상태 | api-gateway, ws-gateway | `redis_rate` 관리 | HTTP 요청과 티켓 발급 제한 |
| `wss:member:{instanceAddr}` | 인스턴스 등록 토큰 | websocket-service, ws-gateway | 30초, 10초마다 갱신 | WebSocket Service 후보 목록 |
| `wss:room:lease:{roomID}` | 담당 인스턴스 주소, 소유권 토큰 | websocket-service | 30초, 10초마다 갱신 | 채팅방 Hub 소유권 |
| `wss:room:seqfloor:{roomID}` | 마지막 발급 순번 | websocket-service | 만료 없음, 더 큰 값으로만 갱신 | 저장 실패 후 순번 재사용 방지 |

Refresh Token 상태는 중간 상태가 생기면 안 되므로 Lua 스크립트로 한 번에 바꿉니다. user-service가 새 토큰을 발급하면 Redis는 기존 active 키를 삭제하고, 기존 토큰의 used 키와 새 토큰의 active 키를 생성합니다. 사용자별 유효 토큰 해시 목록도 함께 갱신해, 로그아웃과 회원탈퇴 시 폐기 대상을 찾을 수 있게 합니다. 토큰 원문은 Redis에도 저장하지 않고, SHA-256으로 해시한 값을 키에 사용합니다. 사용 완료 표식이 있는 토큰이 다시 들어오면 재사용 공격으로 보고 해당 사용자의 유효 Refresh Token을 모두 폐기합니다.

WebSocket Service 후보 목록은 변경 감지와 목록 재구성을 분리합니다. Keyspace notification은 후보 변경 신호만 제공하고 전체 후보 목록은 제공하지 않으므로, 이벤트를 받으면 `SCAN wss:member:*`로 현재 Redis에 남아 있는 키를 다시 읽습니다. 주기적 재검사도 함께 수행해 재시작이나 네트워크 끊김 중 놓친 이벤트를 보완합니다.

채팅방 소유권은 Hub가 아니라 Hub Manager가 관리합니다. Manager는 자신이 보유한 방 lease를 10초마다 모아 Redis pipeline으로 갱신합니다. 각 lease 갱신은 Lua 스크립트로 소유권 토큰을 확인한 뒤 TTL을 연장하므로, 다른 인스턴스가 획득한 lease를 덮어쓰지 않습니다.

처리율 제한은 애플리케이션이 제한 기준만 정하고, Redis 키/값 구조는 `redis_rate` 라이브러리에 맡깁니다. 애플리케이션은 클라이언트 IP나 사용자 식별자를 제한 기준으로 넘길 뿐, `rate:` 내부 스키마에는 직접 의존하지 않습니다.

#### 스키마 마이그레이션

스키마와 인덱스는 마이그레이션 파일에서만 관리합니다. 실행 코드가 시작할 때 인덱스를 자동 생성하지 않게 해, 환경마다 스키마가 달라지는 상황을 피합니다.

- 개발 환경과 통합 테스트에서 `golang-migrate/migrate` 공통 사용
- 인덱스 생성은 레포지토리 코드에서 제외, 마이그레이션 스크립트에 일임
- 타임스탬프 기반 버전 관리로 파일명 충돌 방지

### 2.5 채팅방 동작

채팅방 참여/나가기는 멤버십을 바꾸는 REST 동작이고, 접속/접속 해제는 WebSocket 연결 상태입니다. 멤버십 변화는 시스템 메시지로 남기지만, 화면 진입이나 연결 종료만으로는 채팅 이력에 메시지를 만들지 않습니다.

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

클라이언트의 WebSocket 요청은 처음에는 일반 HTTP 요청으로 들어오고, 검증이 끝난 뒤 WebSocket 연결로 전환됩니다. WebSocket Service는 이 전환 전에 요청을 처리할 인스턴스와 채팅방 소유권을 확정합니다. 이를 위해 Router, Manager, Hub가 역할을 나눕니다. Router는 티켓과 멤버십, 담당 인스턴스 여부를 확인하고, Manager는 방 소유권과 Hub 생명주기를 관리합니다. Hub는 한 채팅방의 세션, 브로드캐스트, 메시지 순번을 담당합니다.

연결 준비부터 세션 등록까지의 순서는 [WebSocket 연결 시퀀스](diagrams/seq-websocket.mmd)에 정리했습니다.

#### WebSocket 연결 수립

1. 클라이언트가 WS Gateway에 티켓을 제시하여 WebSocket 연결을 요청합니다.
2. WS Gateway가 티켓을 검증하고, consistent hashing으로 대상 WebSocket Service 인스턴스를 선택하여 프록시합니다.
3. WebSocket Service의 Router가 User Service에 멤버십 검증을 요청하고, 자기 해시 링 기준 담당 인스턴스인지 확인합니다.
4. Router가 upgrade 전에 `Manager.PrepareRegister`를 호출합니다. 이 단계에서 기존 Hub를 찾거나, 새 Hub를 만들기 위해 room lease를 먼저 획득합니다.
5. 새 Hub가 필요하면 Manager는 room lease 획득 후 `max(DB 마지막 순번, Redis sequence floor)`로 시작 순번을 먼저 초기화합니다. 이 초기화가 실패하면 Hub를 시작하지 않고 가능한 범위에서 lease를 반납합니다.
6. room lease가 사용 중이거나 담당 인스턴스 이전 중이거나 순번 초기화에 실패하면 upgrade하지 않고 `503 Service Unavailable`과 `Retry-After: 1`을 반환합니다.
7. 준비가 끝난 뒤에만 WebSocket upgrade를 수행하고, upgrade 성공 시 `Commit`으로 Hub에 세션을 등록합니다. upgrade 또는 commit 실패로 새 Hub에 세션이 없으면 lease를 반납하고 Hub를 닫습니다.
8. Hub가 세션을 생성하고 읽기/쓰기 펌프를 가동합니다.

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

WebSocket은 실시간 전송만 담당합니다. 저장된 메시지 조회와 누락분 동기화는 REST API가 맡습니다. 그래서 실시간 연결이 잠시 끊겨도 클라이언트는 `sequence_number`를 기준으로 빠진 메시지를 다시 맞출 수 있습니다.

메시지 수신부터 저장 완료 처리까지의 흐름은 [메시지 처리 흐름](diagrams/flow-message.mmd)에 정리했습니다.

#### 전송 및 브로드캐스트

1. 클라이언트가 `client_msg_id`를 담아 메시지를 보냅니다.
2. Session의 `readPump`에서 처리율 제한과 필수 필드(`content`, `client_msg_id`)를 확인합니다.
3. Hub의 LRU 캐시에서 `client_msg_id`를 검사해 중복이면 버립니다.
4. Hub가 저장 큐 슬롯을 먼저 확보합니다. 슬롯을 확보하지 못하면 보낸 세션에 일시 오류를 보내고 순번을 증가시키지 않습니다.
5. 슬롯 확보 후 Hub 메모리에서 `sequence_number`를 증가시키고, 방의 모든 참여자에게 브로드캐스트합니다. 전송 버퍼가 가득 찬 세션에는 해당 메시지를 쓰지 않고 다음 처리를 계속합니다. 이 세션은 이후 REST 동기화로 누락 구간을 보충합니다.

#### 멱등성과 저장

브로드캐스트는 저장 작업이 큐에 들어간 것을 확인한 뒤 수행합니다. 실제 저장은 비동기 배치 워커가 처리하지만, 클라이언트가 받은 메시지는 최소한 저장 파이프라인에 들어간 상태입니다. 저장 큐가 가득 차면 순번을 부여하지 않고 메시지를 거절하므로, 먼저 전달하고 나중에 저장만 실패하는 상태를 만들지 않습니다.

- 저장 실패 시 재시도 워커가 무작위 지연을 섞은 지수 백오프로 최대 5회 재시도합니다.
- 재시도 중 `{ roomId, clientMsgId }` 유니크 인덱스 충돌이 나면 이미 저장된 메시지로 보고 성공 처리합니다.
- `{ roomId, sequenceNumber }` 유니크 충돌은 방 소유권 이전 정합성 실패로 보고 오류 로그와 `gochat_ws_sequence_conflict_total`을 남깁니다.

#### 메시지 동기화

메시지 조회 API(`GET /rooms/{id}/messages`)는 `last_seq` 쿼리 파라미터 유무에 따라 두 방식으로 동작합니다.

- `last_seq` 없음: 최근 메시지를 `limit`만큼 로드. 처음 입장하거나 오래 비운 뒤 사용
- `last_seq` 있음: 해당 시퀀스 이후의 누락분을 시간순으로 보충. 재연결 시 사용

클라이언트는 로컬에 저장한 `last_seq`를 기준으로 동작합니다. 처음 방에 들어가면 최근 메시지를 불러와 가장 큰 `sequence_number`를 저장하고, 실시간 메시지를 받을 때마다 이 값을 갱신합니다. 재연결 후에는 `last_seq` 이후의 누락분을 REST API로 보충한 뒤, 연결이 끊긴 동안 보류한 메시지를 기존 `client_msg_id`로 다시 전송합니다.

#### 메시지 작성자 표시

메시지는 작성자 ID만 저장하고 username은 저장하지 않습니다. 클라이언트는 방 입장 시 멤버 목록으로 `user_id` → username 캐시를 만들고, 메시지에서 처음 보는 작성자 ID만 `GET /users?ids=...`로 일괄 조회합니다. 현재는 username 변경 기능이 없으므로 주기적 갱신은 두지 않았습니다. 이후 username 변경을 지원하면 캐시 만료나 프로필 변경 이벤트를 별도로 설계해야 합니다.

방을 떠난 사용자의 메시지도 사용자 행이 살아있으면 정상적으로 username을 반환받습니다. 회원탈퇴로 사용자 행이 삭제된 사용자는 일괄 조회 응답에서 제외되며, 클라이언트가 `(탈퇴한 사용자)` 대체 문구로 표시합니다. 메시지 저장 모델이 사용자 프로필에 직접 의존하지 않기 때문에, 작성자 표시 실패가 메시지 조회 자체를 깨뜨리지 않습니다.

### 2.9 설정 관리

애플리케이션 이미지는 환경과 무관하게 동일하게 빌드하고, 실행 환경의 차이는 Kustomize overlay로 주입합니다. 앱 설정의 `ENV`는 `dev`, `test`, `qa`로 나뉩니다. `test` overlay는 부하 테스트가 처리율 제한에 먼저 막히지 않도록 제한 값을 더 완화합니다.

설정은 앱이 실제로 읽는 형태를 기준으로 나눕니다.

| 경로 | 담는 값 |
| :--- | :--- |
| ConfigMap `base.yaml` | 포트, 타임아웃, 서비스 주소처럼 공개 가능한 기본값 |
| ConfigMap `override.yaml` | 환경 이름, 관측성 엔드포인트, `test` 처리율 제한 |
| 환경변수 | 인증 Secret, DB 접속 정보, Redis에 등록할 WebSocket Service 주소 |

각 서비스는 ConfigMap 파일을 먼저 읽고, `APP_` 환경변수를 병합한 뒤 설정 구조체를 검증합니다. 공통 설정 타입은 `internal/shared/config`에 두고, 서비스별 설정 트리는 각 서비스 패키지에 둡니다. 필수 값이 비어 있거나 범위를 벗어나면 서비스는 시작되지 않습니다.

Secret YAML은 앱 설정 파일이 아니라 Kubernetes 리소스 정의입니다. Kubernetes는 이 정의로 Secret을 만들고, 컨테이너에는 `envFrom.secretRef`로 `APP_JWT_SECRET`, `APP_DB_POSTGRES_URL` 같은 환경변수를 주입합니다. WebSocket Service는 Downward API로 받은 Pod IP를 `APP_WEBSOCKET_ADVERTISED_ADDR`로 만들어 Redis 후보 등록에 사용합니다.

K8s가 자동으로 넣는 Service 환경변수에는 `APP_` prefix가 없으므로 앱 설정을 덮어쓰지 못합니다. 채널 크기와 배치 크기처럼 환경에 따라 바뀌지 않는 내부 한계값은 설정 파일로 빼지 않고 Go 상수로 둡니다.

### 2.10 우아한 종료

각 서비스는 `errgroup`으로 종료 순서를 관리합니다. HTTP/gRPC 서버는 새 요청을 막고 진행 중인 요청을 기다리면 되지만, WebSocket Service는 열린 연결과 비동기 저장 큐를 함께 정리해야 합니다. 그래서 HTTP 서버 종료와 별도로 Hub/Manager 레벨의 drain 절차를 둡니다.

1. 종료 신호를 받으면 HTTP 서버가 새 요청 수신을 중단하고, 진행 중인 HTTP 요청 완료를 기다립니다.
2. Manager가 모든 Hub를 drain 상태로 전환합니다. Hub는 새 세션 등록과 새 브로드캐스트를 거절하되, 이미 `broadcastCh`에 들어온 메시지는 끝까지 브로드캐스트합니다.
3. 브로드캐스트된 메시지는 저장을 위해 `persistCh`에 전달됩니다. Hub는 이미 받은 메시지의 브로드캐스트와 저장 요청이 마무리될 때까지 기다리며, 제한 시간을 넘으면 timeout으로 기록하고 다음 단계로 진행합니다.
4. Hub가 멈추면 Manager는 자기 소유권 토큰이 맞는 경우에만 Redis room lease를 반납합니다. 저장 실패가 확정된 경우에는 마지막 발급 순번을 Redis sequence floor에 먼저 기록해, 새 담당 인스턴스가 같은 순번을 다시 쓰지 않게 합니다.
5. 모든 Hub가 멈춘 뒤 Manager는 `persistCh`를 닫고, 배치 워커와 재시도 워커가 남은 저장 작업을 처리할 때까지 기다립니다.
6. 마지막으로 gRPC 연결과 텔레메트리 수집기를 정리합니다.

Go의 `http.Server.Shutdown`은 WebSocket처럼 hijack된 연결을 기다리지 않습니다. 그래서 WebSocket 연결 정리는 HTTP 서버가 아니라 Hub/Manager가 담당합니다. 제한 시간 안에 drain이 끝나지 않아도 종료는 계속 진행하고, timeout 지표와 room_id 로그를 남겨 어느 방에서 지연됐는지 확인할 수 있게 합니다.

---

## 3. 주요 의사결정

### 3.1 WebSocket 라우팅

WebSocket 메시지는 같은 방의 세션을 한 WebSocket Service 인스턴스로 모아 브로드캐스트합니다. 메시지마다 Redis Pub/Sub를 거치지 않고, WS Gateway가 `room_id` 기반 consistent hashing으로 대상 인스턴스를 고릅니다.

이 구조의 목적은 메시지 분배 지연을 낮추고 중앙 병목을 피하는 것입니다. 방 단위 브로드캐스트와 순번 부여는 담당 WebSocket Service 인스턴스의 로컬 메모리에서 처리하고, Redis는 라우팅 후보 목록과 방 소유권 같은 제어 상태만 맡습니다. 여기서 방 소유권은 특정 방을 현재 어느 인스턴스가 처리하는지를 뜻합니다.

WebSocket Service 인스턴스는 Redis에 자기 주소를 `POD_IP:PORT` 형식으로 등록합니다. Service VIP를 거치면 consistent hashing이 고른 담당 인스턴스가 Kubernetes Service 로드밸런싱으로 다시 바뀔 수 있기 때문입니다.

WS Gateway는 Redis의 후보 키 변화를 감지해 해시 링을 갱신합니다. Keyspace notification은 변경 신호로 사용하고, 실제 후보 목록은 `SCAN wss:member:*`로 다시 읽습니다. 30초 주기 재검사도 함께 둬 이벤트를 놓쳐도 Redis에 남아 있는 후보 목록으로 해시 링을 다시 맞춥니다.

라우팅 경로에서 Redis가 맡는 상태는 세 가지입니다.

| 상태 | 책임 |
| :--- | :--- |
| 멤버십 | 해시 링 후보 인스턴스 목록 |
| 방 소유권 | 한 방의 Hub가 동시에 여러 인스턴스에서 열리지 않게 보호 |
| 마지막 순번 기준값 | 저장 실패 후 순번 재사용 방지 |

정확한 키 이름, TTL, 책임 서비스는 2.4의 Redis 저장소 표에 정리되어 있습니다.

멤버십 주소는 같은 값이 짧은 시간 안에 다시 등록될 수 있으므로, 값에는 프로세스별 토큰을 넣습니다. 종료 시에는 자기 토큰이 맞을 때만 Lua 스크립트로 삭제해 새 프로세스의 멤버십을 지우지 않게 합니다.

방 소유권은 기존 Hub가 신규 수신을 막고, 이미 받은 메시지의 저장 대기 작업을 끝낸 뒤에만 반납됩니다. 저장 실패가 확정된 경우에는 마지막 발급 순번을 `wss:room:seqfloor:{roomID}`에 기록합니다. 새 담당 인스턴스는 이 값을 함께 보고 시작 순번을 정하므로, 이미 발급된 순번을 다시 쓰지 않습니다.

전체 라우팅 구조와 정적 라우팅 대비 차이는 [WebSocket 라우팅 비교](diagrams/flow-ws-routing.mmd)에 정리했습니다.

### 3.2 분산 라우팅 정합성

WS Gateway와 WebSocket Service의 각 인스턴스는 Redis 후보 목록을 관찰해 자기 로컬 해시 링을 갱신합니다. 하지만 모든 인스턴스가 같은 순간에 같은 목록을 보는 것은 아닙니다. 그래서 WS Gateway의 해시 링과 WebSocket Service의 해시 링이 일시적으로 다를 수 있다는 전제로 방어합니다.

후보 목록이 각 인스턴스의 로컬 해시 링에 반영되는 과정은 [멤버십 동기화 시퀀스](diagrams/seq-membership-sync.mmd)에 정리했습니다.

| 상황 | 처리 | 이유 |
| :--- | :--- | :--- |
| 잘못된 담당 인스턴스로 라우팅 | WebSocket Service가 421 응답, WS Gateway가 503으로 변환 | 오래된 라우팅 정보를 빠르게 드러내고 클라이언트가 새 담당 인스턴스로 다시 연결 |
| 새 Hub 생성 | WebSocket upgrade 전에 방 소유권 획득 및 순번 초기화 | 소켓을 열기 전에 담당 인스턴스와 순번 기준을 확정 |
| 스케일아웃 시 재배치 | 0~2초 무작위 지연 후 기존 연결 종료 | 재접속 트래픽 집중 완화 |
| 담당 인스턴스 변경 | 저장 대기 작업 완료 후 방 소유권 반납 | 새 담당 인스턴스가 이전 저장 완료 전에 순번을 시작하지 않게 함 |
| 방 소유권 갱신 실패 | 토큰 불일치나 키 없음은 즉시 Hub 종료, Redis 오류는 마지막 성공 갱신 후 20초 초과 시 Hub 종료 | 일시 오류는 흡수하되 소유권이 불확실해진 상태에서는 메시지 수신 차단 |
| 빈 후보 목록 관측 | 기존 해시 링은 유지, readiness는 별도 판단 | Redis 일시 오류와 실제 후보 없음 구분 |

담당 인스턴스 변경은 멤버십 변경 이벤트를 받으면 바로 검사하고, 기존 Hub는 0~2초 무작위 지연 후 종료 절차에 들어갑니다. 이벤트를 놓친 경우에도 Manager가 10초마다 자신이 가진 Hub의 담당 여부를 다시 확인하므로, 최대 약 12초 안에 종료 절차가 시작됩니다. 이후 Hub는 이미 브로드캐스트한 메시지의 저장 완료를 기다린 뒤 방 소유권을 반납합니다.

스케일아웃 중 담당 인스턴스가 바뀌는 흐름은 [스케일아웃 시 재배치](diagrams/seq-rebalance.mmd)에 정리했습니다.

WebSocket 연결 라우팅 실패에 대해서는 WS Gateway나 WebSocket Service가 다른 인스턴스로 서버 측 재시도를 하지 않습니다. 연결 재시도는 클라이언트가 무작위 지연을 섞어 수행하게 해 재시도 트래픽이 한 번에 몰리지 않게 합니다.

421/503 변환 흐름은 [담당 인스턴스 자가 확인](diagrams/seq-owner-self-check.mmd)에 정리했습니다.

HashRing의 `Set`은 여러 후보 추가/삭제를 한 번에 반영하는 복합 작업입니다. 중간 상태에서 잘못된 담당 인스턴스가 나오지 않도록 HashRing 내부에서 `Set`은 Lock, `Locate`는 RLock으로 보호합니다.

#### Liveness와 Readiness 분리

`/health`는 프로세스 생존 확인용으로 단순 200을 반환합니다. `/ready`는 트래픽을 받아도 되는지 확인하므로 의존성 상태를 더 엄격하게 봅니다.

| 서비스 | `/ready` 검사 |
| :--- | :--- |
| api-gateway | Redis `PING`, user/chat gRPC health |
| ws-gateway | Redis `PING`, 후보 목록 관측 여부, 해시 링 후보 존재 |
| websocket-service | Redis `PING`, user/chat gRPC health, Manager 동작 상태, 자기 주소가 포함된 해시 링, 저장 큐 사용률 80% 미만 |

user-service와 chat-service의 gRPC health는 각각 PostgreSQL `Ping`, MongoDB `Ping` 결과를 주기적으로 반영합니다. HTTP gateway가 DB를 직접 확인하지 않고, 각 서비스가 자기 의존성 상태를 gRPC Health Checking Protocol로 노출하는 구조입니다.

#### Redis를 메시지 경로에서 제외한 이유

채팅 메시지는 사용자 입력마다 발생하고 지연이 바로 체감되므로, 메시지마다 Redis Pub/Sub 왕복을 추가하면 Redis가 중앙 병목이 될 수 있습니다. 그래서 실제 브로드캐스트는 담당 Hub의 로컬 메모리에서 처리합니다.

반대로 Redis에는 라우팅을 맞추기 위한 제어 상태만 둡니다. WebSocket Service 후보 목록, 방 소유권, 마지막 순번 기준값은 사용자 메시지보다 변경 빈도가 낮고, 일시적으로 늦게 반영돼도 421/503 응답, 주기적 재검사 등으로 복구할 수 있습니다.

#### Redis keyspace notification 옵션

후보 목록 관찰자는 `__keyspace@<db>__:wss:member:*` 채널을 구독합니다. Redis 옵션은 SET/DEL/expired 이벤트만 켜는 `K$gx`로 제한해 멤버십 키 변화만 관측합니다.

### 3.3 WebSocket 계층 구조

WebSocket Service는 연결 수명, 방 단위 브로드캐스트, 메시지 저장 요청을 함께 다룹니다. 이 책임을 한 계층에 모으면 상태 변경 순서를 추적하기 어려워지므로 Router, Manager, Hub, Session으로 나눴습니다. 의존 방향은 위에서 아래로만 흐르게 제한했습니다.

```
Router (1)
└── Manager (1)
    ├── Hub (채팅방 A)
    │   ├── Session (유저 1)
    │   └── Session (유저 2)
    └── Hub (채팅방 B)
        └── Session (유저 3)
```

Manager와 Hub는 액터 모델로 동시성을 처리합니다. Manager는 Hub 목록과 생명주기를 단일 `select` 루프에서 관리하고, Hub는 세션 목록, 순번 부여, 브로드캐스트를 자기 루프에서 순서대로 처리합니다. 외부와는 채널 또는 주입된 함수로만 통신해 공유 상태를 직접 잠그는 범위를 줄였습니다. 다만 Hub가 종료 절차에 들어간 뒤 새 메시지가 `broadcastCh`에 들어가지 않도록 `RWMutex`와 atomic flag로 수신 경계를 닫습니다.

#### 계층별 책임

| 계층 | 책임 |
| :--- | :--- |
| Router | HTTP 요청을 검증하고 WebSocket upgrade 전까지의 준비 절차를 조율 |
| Manager | Hub 생성과 종료, 방 소유권, 저장 파이프라인을 관리 |
| Hub | 한 방의 세션 목록, 순번 부여, 브로드캐스트를 직렬 처리 |
| Session | 개별 WebSocket 연결의 읽기/쓰기와 메시지 검증을 담당 |

#### 부모 직접 참조 차단

자식 계층이 부모를 직접 참조하면 순환 의존이 생깁니다. 상향 통신이 필요한 경우에도 자식이 부모의 구체 타입을 모르도록 제한합니다.

상향 통신 방식은 호출 성격에 따라 나눴습니다. 부모의 작업 큐로 넘기는 일은 송신 전용 채널로, 호출한 자리에서 바로 결과가 필요한 일은 콜백 함수로 분리했습니다.

| 방식 | 쓰는 기준 | 이 프로젝트의 예 |
| :--- | :--- | :--- |
| 송신 전용 채널 | 자식이 부모의 작업 큐나 이벤트 루프에 일을 맡길 때 | Hub가 저장 작업을 `persistCh`로 전달 |
| 콜백 함수 | 호출한 자리에서 바로 허용 여부나 처리 결과가 필요할 때 | Session이 메시지 publish 함수와 rate-limit 함수를 호출 |

콜백으로 둔 동작은 부모 액터 루프에서 반드시 직렬화해야 하는 상태 변경이 아닙니다. 메시지 발행 위임이나 처리율 제한 검사처럼 호출한 자리에서 결과를 받아 다음 동작을 정하면 되므로, 채널 요청으로 만들지 않고 함수 주입으로 단순하게 유지했습니다.

### 3.4 비동기 배치 저장

메시지를 브로드캐스트와 동시에 저장하면 저장 지연이 실시간 전송 경로에 직접 영향을 줍니다. 그래서 실시간 분배와 저장을 분리하고, 저장은 배치 워커에서 비동기로 처리합니다.

분리하더라도 저장 경로가 꽉 찬 메시지를 클라이언트에 먼저 전달하지는 않습니다. Hub는 `persistCh`에 작업을 넣기 전에 같은 크기의 버퍼드 채널을 세마포어처럼 사용해 저장 경로에 들어갈 자리를 먼저 확보합니다. 자리를 확보한 뒤에만 순번을 부여하고, 저장 작업을 `persistCh`에 넣은 다음 브로드캐스트합니다. 저장 경로가 꽉 차 있거나 중간 단계에서 실패하면 순번과 예약을 되돌리고 메시지를 보낸 세션에 일시 오류를 반환합니다.

저장 워커는 채널에서 작업을 꺼내 일정 건수 또는 타이머 기준으로 배치 저장합니다. 메시지 순서는 Hub가 이미 부여하므로 DB 저장 순서는 정합성 기준이 아닙니다. 이미 저장된 메시지는 성공으로 보고, 재시도 가능한 오류만 재시도 큐로 보냅니다.

이 구조는 브로드캐스트 지연을 낮추는 대신, 저장 실패가 길어지면 Hub 종료가 늦어질 수 있습니다. 서버 종료나 방 담당 인스턴스 변경 시 Hub는 이미 브로드캐스트한 메시지의 저장 완료를 기다리지만, `shutdown_timeout`을 넘으면 지표와 로그를 남기고 종료를 계속합니다.

### 3.5 서비스 간 통신

#### gRPC 선택

내부 서비스 간 호출은 REST 대신 gRPC로 통일했습니다. 외부 API는 사람이 읽고 디버깅하기 쉬운 HTTP/JSON 계약이 중요하지만, 내부 호출은 서비스 간 타입 계약과 호출부 일관성이 더 중요하다고 봤습니다.

`.proto` 파일은 서비스 간 계약입니다. 요청/응답 필드와 메서드 시그니처가 생성 코드에 반영되므로, 계약 변경 후 서버와 클라이언트가 맞지 않으면 컴파일 시점에 드러납니다. 반복적인 요청 파싱과 직렬화 코드를 직접 작성하지 않아도 되는 점도 선택 이유였습니다.

각 서비스는 요청마다 연결을 새로 만들지 않고, 대상 서비스마다 `grpc.ClientConn`을 공유합니다. `ClientConn`은 단일 TCP 연결 하나라기보다 대상별 HTTP/2 연결과 로드밸런싱 상태를 관리하는 클라이언트 객체에 가깝습니다. Headless Service가 반환한 대상 Pod IP 목록을 gRPC resolver가 보고, `round_robin` 정책으로 RPC를 분산합니다.

이 방식은 고정 replica에서는 단순하지만, HPA 확장까지 맡기기에는 부족합니다. 기존 `ClientConn`이 정상 연결을 유지하는 동안 새 Pod를 찾기 위한 DNS 재조회가 보장되지 않으므로, scale-out 후에도 트래픽이 기존 Pod에만 분산될 수 있습니다. user-service와 chat-service까지 HPA 대상으로 삼으려면 동적 endpoint 갱신을 안정적으로 처리할 중간 로드밸런싱 계층 도입을 검토해야 합니다.

### 3.6 bcrypt 워커 풀

bcrypt는 무차별 대입을 어렵게 만들기 위해 의도적으로 느리게 동작하는 CPU 바운드 해시 알고리즘입니다. DB나 네트워크 I/O는 응답을 기다리는 동안 고루틴이 block되어 다른 고루틴이 CPU를 사용할 수 있지만, bcrypt는 실행되는 동안 CPU를 계속 사용합니다. Go 런타임이 동시에 실행할 수 있는 Go 코드는 `GOMAXPROCS` 범위로 제한되므로, CPU 바운드 작업은 많이 시작한다고 처리량이 그만큼 늘지 않습니다.

가입이나 로그인 요청이 몰릴 때 모든 요청 고루틴이 bcrypt를 바로 수행하면, 코어 수보다 많은 해싱 작업이 한정된 CPU 시간을 나눠 갖습니다. 작업 하나가 빨리 끝나기보다 여러 작업이 동시에 조금씩 진행되면서 대기열이 길어집니다. 이 상태가 길어지면 인증 요청뿐 아니라 같은 인스턴스의 다른 요청도 함께 밀립니다.

그래서 bcrypt 연산에는 워커 풀 패턴을 도입했습니다. 워커 수는 `runtime.GOMAXPROCS(0)` 기준으로 두어 동시에 실행되는 해싱 수를 실제 병렬 실행 폭에 맞춥니다. 짧은 순간의 요청 증가는 제한된 큐로 흡수하되, 큐가 가득 차면 더 기다리게 하지 않고 `ErrQueueFull`을 반환합니다.

이 선택은 일부 요청을 빠르게 실패시키는 대신, CPU 경합이 user-service 인스턴스 전체로 번지는 것을 막는 쪽에 가깝습니다. user-service는 `ErrQueueFull`을 `ResourceExhausted`로 바꿔 gateway가 과부하 응답을 낼 수 있게 합니다. 과부하가 런타임의 고루틴 대기열에 숨지 않고, 큐 깊이와 queue full 지표로 관측 가능한 backpressure가 되도록 한 것입니다.

### 3.7 ID 생성 위치와 UUID v7

사용자, 채팅방, 메시지 같은 주요 ID는 DB가 아니라 애플리케이션에서 UUID v7로 생성합니다. ID 생성 시점과 저장 시점을 분리해, 저장 전에 로그와 응답 객체에서 같은 식별자를 사용할 수 있게 하기 위해서입니다.

UUID v7은 생성 시각을 포함하므로 대략적인 시간순 정렬과 로그 추적에 유리합니다. UUID v4보다 B-tree 삽입 위치가 덜 분산되어 인덱스 관리에도 부담이 적습니다. 동시에 auto-increment처럼 전체 레코드 수나 생성 속도를 외부에서 쉽게 추측하게 만들지 않습니다.

애플리케이션에서 ID를 만들면 INSERT 전에 식별자가 확정됩니다. 그래서 관련 엔티티의 FK를 DB 왕복 없이 설정할 수 있고, 비동기 저장 파이프라인에서도 브로드캐스트와 저장을 분리하기 쉽습니다. user-service와 chat-service는 UUID v7의 시간을 `created_at` 기준으로도 사용해 ID와 생성 시각이 어긋나지 않게 합니다.

다만 UUID v7의 시간성은 운영 중 추적과 인덱스 locality를 위한 선택입니다. 채팅방 안의 메시지 순서는 ID가 아니라 Hub가 부여하는 `sequence_number`로 판단합니다.

---

## 4. Kubernetes 배포 설계

K8s 전환의 목표는 서비스 인스턴스 수가 동적으로 변해도 서비스 동작과 데이터 정합성이 유지되는지 검증하는 것입니다. Kubernetes에서는 서비스 인스턴스가 Pod 단위로 생성·종료되고 트래픽 대상에서 제외됩니다. 애플리케이션은 readiness와 종료 절차로 트래픽 수신 가능 상태를 명확히 드러내야 합니다.

리소스 구분은 다음과 같습니다.

| K8s 리소스 | 역할 | 선택 이유 |
| :--- | :--- | :--- |
| Deployment | 지속 실행 서비스 | replica 수 조절과 rolling update 대상 |
| Service | 안정적인 내부 진입점 | 파드 IP 변경을 DNS 이름 뒤로 숨김 |
| Headless Service | gRPC 대상 발견 | 고정 replica에서 Pod 목록을 직접 보고 `round_robin` 수행 |
| Job | 일회성 마이그레이션 | 성공/실패와 완료 상태가 명확함 |
| Ingress | 외부 HTTP/WebSocket 진입점 | 외부 경로를 서비스로 라우팅 |

K8s 안에서 각 워크로드와 데이터 계층이 배치되는 구조는 [K8s 런타임 배포 구조](diagrams/flow-k8s-runtime.mmd)에 정리했습니다.

### 4.1 Manifest 구조

K8s manifest는 `base`와 overlay로 나눕니다. `base`에는 서비스 구조와 probe처럼 공통인 리소스를 두고, overlay에는 replica 수와 리소스 정책처럼 환경별 차이를 둡니다.

| Overlay | 목적 | 특징 |
| :--- | :--- | :--- |
| `dev` | 로컬 개발과 C10K 부하 확인 | 앱은 대부분 1 replica, `websocket-service`는 C10K 기준에 맞춰 2 replicas |
| `test` | 자동화된 K8s 전체 시나리오 검증 | 주요 gateway/service `replicas: 2`, 메모리 request/limit으로 테스트 격리 |
| `qa` | WebSocket HPA 확장 중 담당 Pod 이전 검증 | WebSocket HPA `1→2` 정합성 검증 |

`local` 대신 `dev/test/qa`로 나눈 이유는 실행 위치가 아니라 검증 목적을 드러내기 위해서입니다. 각 overlay의 차이는 표처럼 실행 목적에 맞춰 제한합니다.

### 4.2 Phase Overlay와 Bootstrap 스크립트

로컬 K8s 환경의 부트스트랩 순서는 [bootstrap.sh](../deploy/k8s/scripts/bootstrap.sh)가 제어합니다. 스크립트는 `K8S_ENV`에 맞는 overlay를 선택하고, phase별 Kustomize overlay를 순서대로 적용한 뒤 각 단계의 준비 상태를 기다립니다. manifest를 한 번에 적용하면 DB 준비나 migration 완료 전에 앱이 뜰 수 있으므로, 데이터 계층, migration, 앱 rollout을 명시적으로 직렬화했습니다.

| Phase | 스크립트 동작 | 완료 조건 |
| :--- | :--- | :--- |
| `foundation` | Secret과 Postgres/Mongo/Redis overlay 적용 | 데이터 계층 Deployment rollout 완료 |
| `observability` | Grafana 스택 ConfigMap 생성 후 observability overlay 적용 | 관측성 Deployment rollout 완료 |
| `migrations` | migration ConfigMap 생성, 기존 Job 삭제, migration overlay 적용 | `postgres-migrate`, `mongo-migrate` Job 완료 |
| `apps` | OpenAPI ConfigMap 생성, apps overlay 적용, 핵심 백엔드 → WebSocket Service → 진입 계층 순서로 rollout restart | 앱 Deployment rollout 완료 |

환경별 overlay 관계는 [K8s overlay 구조](diagrams/flow-k8s-overlays.mmd), 단계별 적용 순서는 [K8s bootstrap 흐름](diagrams/flow-k8s-bootstrap.mmd)에 정리했습니다.

마이그레이션이 실패하면 앱 rollout을 진행하지 않습니다. 스키마가 불확실한 상태에서 앱을 띄우지 않기 위한 즉시 중단 전략입니다.

부하 검증은 [load.sh](../deploy/k8s/scripts/load.sh)가 별도로 실행합니다. 환경을 띄우는 일과 부하를 거는 일을 분리해야 실패 원인을 좁히기 쉽기 때문입니다. `load.sh`는 k6 ConfigMap을 만들고 기존 Job을 삭제한 뒤 load overlay를 적용합니다. QA에서는 HPA 테스트 전 `websocket-service`를 1 replica로 되돌리고 HPA를 다시 붙여 스케일아웃을 매번 같은 시작 상태에서 검증합니다.

### 4.3 Runtime 리소스 모델

계속 요청을 처리하는 서비스는 Deployment로 둡니다.

| Deployment | 역할 | Service |
| :--- | :--- | :--- |
| `api-gateway` | REST API 진입점 | ClusterIP |
| `ws-gateway` | WebSocket ticket/API 및 WebSocket reverse proxy | ClusterIP |
| `websocket-service` | 실제 WebSocket 세션과 방별 Hub 관리 | 없음 |
| `user-service` | 사용자/방/멤버십 gRPC 서비스 | Headless |
| `chat-service` | 메시지 저장/조회 gRPC 서비스 | Headless |
| `frontend` | 정적 프론트엔드 | ClusterIP |
| `swagger-ui` | OpenAPI 문서 UI | ClusterIP |

`api-gateway`, `ws-gateway`, `frontend`, `swagger-ui`는 ClusterIP Service를 사용합니다. Ingress나 내부 호출자는 안정적인 DNS 이름만 필요하고, 특정 Pod를 직접 고를 필요가 없습니다.

`websocket-service`는 의도적으로 Service를 두지 않습니다. WebSocket 방 담당 Pod 라우팅은 Redis 멤버십에 등록된 Pod IP로 직접 들어갑니다. Service VIP를 거치면 consistent hashing이 고른 담당 Pod가 Kubernetes Service 로드밸런싱으로 다시 바뀔 수 있습니다.

`user-service`와 `chat-service`는 Headless Service를 사용합니다. gRPC 클라이언트가 대상 Pod IP 목록을 직접 보고 `round_robin`으로 RPC를 분산하기 위해서입니다.

base manifest에는 resource request/limit을 넣지 않습니다. 리소스 정책은 실행 목적에 따라 달라지므로 overlay에서만 추가합니다.

`dev`는 Compose 시절 C10K 기준과 비교할 수 있게 앱 메모리 limit을 두지 않습니다. `websocket-service`는 2 replicas로 시작하고, `user-service`만 bcrypt 워커 풀 검증용 CPU limit 4를 둡니다. TCP accept queue 관련 sysctl은 주요 서비스에 적용합니다.

`test`는 성능 기준이 아니라 반복 가능한 전체 시나리오 검증 환경입니다. gateway/service 계열은 2 replicas로 고정하고, 로컬 머신 전체 메모리를 잠식하지 않도록 memory request/limit을 둡니다.

`qa`는 WebSocket Service 확장 중 메시지 정합성을 보는 환경입니다. 앱 메모리 limit은 제거하고, `websocket-service`만 활성 연결 수 기준 HPA `1→2`를 겁니다. 담당 Pod 이전 자체가 성능 지표를 흔들 수 있으므로, 합격 기준은 P99 성능이 아니라 순번 일관성과 누락 회복 여부입니다.

`user-service`와 `chat-service`는 HPA 대상이 아닙니다. 현재 구조는 Headless Service와 gRPC `round_robin`으로 고정 replica 분산만 검증합니다. 두 서비스를 HPA 대상으로 확장하려면 동적 endpoint 갱신을 안정적으로 처리할 중간 로드밸런싱 계층 도입을 검토해야 합니다.

### 4.4 Local Data Layer

dev/test/qa에서는 데이터 저장소도 K8s 안에 띄웁니다. 운영용 DB manifest가 아니라 K8s 실행 경로와 E2E/HPA 정합성 검증을 독립적으로 재현하기 위한 로컬 데이터 계층입니다.

| 리소스 | 구현 | 저장소 | 이유 |
| :--- | :--- | :--- | :--- |
| Postgres | Deployment + ClusterIP | `emptyDir` | 사용자/방 스키마와 마이그레이션 검증 |
| Mongo | Deployment + ClusterIP | `emptyDir` | 메시지 스키마, 인덱스, 누락 메시지 조회 검증 |
| Redis | Deployment + ClusterIP | 메모리 | 상태성 Redis 기능과 라우팅 제어 상태 검증 |

로컬 데이터 계층은 운영용 데이터 보존보다 재현성과 정합성 검증을 우선합니다. 매번 깨끗하게 재생성할 수 있고, E2E 데이터 정리도 단순합니다.

#### 데이터 저장소 이미지 기준

dev/test/qa에서 쓰는 저장소 이미지는 애플리케이션이 의존하는 기능을 기준으로 고정합니다. 최신 기능을 따라가기보다 실행 기준선을 고정해, 애플리케이션 검증 결과가 이미지 변경에 흔들리지 않게 합니다.

| 저장소 | 현재 이미지 | 검증 포인트 |
| :--- | :--- | :--- |
| PostgreSQL | `postgres:17` | 트랜잭션, 행 잠금, `pg_trgm` |
| MongoDB | `mongo:7.0` | 메시지 저장, 유니크 인덱스, TTL 인덱스 |
| Redis | `redis:7-alpine` | TTL, Lua 스크립트, keyspace notification |

운영 배포에서는 저장소 버전 고정과 백업/복구 같은 운영 기준을 별도로 설계합니다.

### 4.5 Ingress와 외부 경로

Ingress는 브라우저와 테스트 클라이언트가 접근하는 외부 경로를 하나로 모읍니다.

| 경로 | 대상 | 목적 |
| :--- | :--- | :--- |
| `/` | `frontend` | 프론트엔드 |
| `/api` | `api-gateway` | REST API |
| `/ws-api` | `ws-gateway` | WebSocket ticket 발급 등 HTTP API |
| `/ws` | `ws-gateway` | WebSocket upgrade |
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

user-service와 chat-service의 gRPC health는 DB 상태를 반영합니다. 이 값을 liveness에 넣으면 DB가 잠깐 느려졌다는 이유로 애플리케이션 Pod를 재시작하는 악순환이 생길 수 있습니다. gRPC 서비스 liveness는 TCP socket으로 두고, readiness에서 DB 의존성을 확인합니다. 준비되지 않은 Pod는 Service 엔드포인트에서 빠져 새 트래픽을 받지 않지만, 프로세스 자체는 재시작하지 않습니다.

WebSocket Service는 종료 조건이 더 엄격합니다. 연결 중인 세션과 저장 중인 메시지가 있기 때문에 Pod가 바로 내려가면 처리 중이던 메시지가 손실될 수 있습니다. 애플리케이션 내부의 우아한 종료 절차는 이미 브로드캐스트한 메시지의 저장 완료를 기다리도록 설계되어 있으므로, K8s Deployment에는 `terminationGracePeriodSeconds`를 주어 이 과정이 끝날 시간을 확보합니다.

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
| `E2E_WS_BASE_URL` | `http://test.gochat.localhost:30080/ws-api` | WebSocket ticket/API |
| `E2E_K8S_NAMESPACE` | `go-chat-test` | readiness/replica/멤버십 검증 대상 |

`test` overlay는 주요 gateway/service를 `replicas: 2`로 고정합니다. HPA를 바로 붙이면 replica 변화와 부하 변화가 섞여 실패 원인을 좁히기 어렵습니다. 고정 replica에서 정합성을 확인한 뒤, `qa` overlay에서 WebSocket HPA 조건을 검증합니다.

E2E 데이터 정리에서는 Redis `FLUSHALL`을 사용하지 않습니다. Redis에는 테스트 데이터와 검증 대상인 멤버십 상태가 함께 있기 때문입니다. Postgres/Mongo는 테스트 데이터만 정리하고, Redis는 `auth:rt:*`, `ws:ticket:*`, `wss:room:lease:*` 등 테스트 요청이 만든 상태성 키만 삭제합니다. `wss:member:*`는 검증 대상이라 보존합니다.

### 4.8 K6 부하와 HPA 정합성 검증

부하 테스트는 목적별로 분리합니다.

| 명령 | 대상 | 시나리오 | 목적 |
| :--- | :--- | :--- | :--- |
| `make dev-load` | `go-chat-dev` | `c10k-test.js`, k6 Pod 4개 | C10K 부하 경로와 기존 Compose 기준 비교 |
| `make qa-load` | `go-chat-qa` | `hpa-test.js`, k6 Job 1개 | `websocket-service` HPA `1→2` 중 메시지 정합성 검증 |

`dev-load`는 많은 연결과 메시지를 넣는 성능/부하 시나리오입니다. k6 자체가 병목이 되지 않도록 Kubernetes Job `parallelism=4`, `completionMode=Indexed`로 k6 Pod 4개를 띄우고, 각 Pod가 VU offset을 나눠 가집니다.

`qa-load`는 C10K 성능 테스트가 아닙니다. 매 실행마다 `websocket-service`를 1 replica에서 다시 시작해 HPA `1→2` 전환을 재현합니다. 이전 실행의 축소 안정화 때문에 2 replicas에서 시작하면 담당 Pod 이전을 검증하지 못합니다. 합격 기준은 HTTP/WebSocket 에러 없이 메시지 순번과 누락 회복이 유지되는지입니다.

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

- Testcontainers로 실제 DB/Redis를 띄우고 시나리오 위주로 검증한다.
- Redis keyspace notification, expire 이벤트, 실제 서버 설정처럼 fake 구현과 운영 Redis의 차이가 의미 있는 동작은 통합 테스트에서 검증한다.
- 데이터 오염을 막기 위해 순차 실행하고, 매 테스트마다 데이터를 초기화한다.

#### E2E 테스트

- K8s `test` overlay로 전체 시스템을 띄우고 블랙박스로 검증한다.
- E2E suite는 이미 bootstrap된 `go-chat-test` namespace의 readiness를 확인한 뒤 실행한다.
- 테스트 간 데이터 정리는 K8s 내부 Postgres truncate와 MongoDB drop으로 수행한다.
- Redis 멤버십과 해시 링 상태는 검증 대상이므로 전체 삭제하지 않는다.
- 시나리오 번호 순서대로 사용자 여정을 이어가며 검증한다.

---

## 6. 관측성

관측성은 장애 위치를 빠르게 좁히기 위한 흐름으로 구성합니다. 메트릭으로 이상 범위를 잡고, 트레이스와 로그로 요청 맥락을 확인하며, 프로파일로 코드 병목을 봅니다.

계측은 OpenTelemetry SDK와 Grafana 스택으로 통일합니다. Alloy는 로그/메트릭/트레이스를 수집하고, 프로파일은 `pyroscope-go`가 Pyroscope로 보냅니다.

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
| 상관관계 | 로그에 `trace_id`/`span_id`를 주입하고, 서비스 간 `traceparent`를 전파 |
| 노이즈 제거 | `/health`, `/ready`, 메트릭 엔드포인트는 로그/트레이스 수집에서 제외 |
| 민감정보 보호 | 토큰/비밀번호/시크릿 쿼리 파라미터 마스킹, DB 쿼리문 노출 통제 |
| 카디널리티 제어 | URL path의 UUID를 `:id`로 정규화 |
| 커버리지 | HTTP/gRPC 경로, 저장소, WebSocket, 인증 경로 계측 |
| 샘플링 | 트레이스는 10% 샘플링 |

수집량도 설계 대상입니다. `/health`, `/ready` 같은 probe 요청은 장애 분석보다 노이즈를 많이 만들기 때문에 로그와 트레이스에서 제외합니다. URL path에 UUID를 그대로 남기면 사용자나 방마다 메트릭 시계열이 늘어나므로 `:id`로 정규화합니다. 트레이스는 10%만 샘플링해 로컬 검증 환경의 비용을 제한합니다.

### 6.3 통합 조회

Grafana 대시보드는 장애 분석 흐름에 맞춥니다. 전체 상태에서 시작해 트래픽과 저장소 지표로 범위를 좁히고, 메시지 흐름과 런타임 지표로 원인을 확인합니다.

| 대시보드 | 내용 |
| :--- | :--- |
| Overview | 서비스 상태, 전체 시스템 헬스 |
| Traffic | HTTP/gRPC 요청률, 지연, 에러율 |
| Database | PostgreSQL, MongoDB 쿼리 성능 |
| Message | WebSocket 메시지 흐름, 저장, 재시도 |
| Runtime | Go 런타임 (GC, 메모리, 고루틴) |
| Infra | 컨테이너 CPU/메모리 사용량 (cAdvisor) |

`trace_id`를 기준으로 Loki 로그와 Tempo 트레이스를 연결합니다. 로그에서 트레이스로, 트레이스에서 로그로 바로 이동할 수 있어 장애 분석 중 화면 전환 비용을 줄입니다.

---

## 7. 검증 범위와 운영 경계

검증 범위는 로컬 kind 기반 K8s `dev`/`test`/`qa` 실행과 WebSocket HPA 정합성 검증까지입니다.

부하 검증 결과는 [Kubernetes C10K 부하 테스트 보고서](K8S_C10K_REPORT.md)에 정리합니다. HPA 확장 중 담당 Pod 이전 정합성 결과는 [README의 WebSocket HPA Consistency](../README.md#websocket-hpa-consistency)에 요약되어 있습니다.

### 7.1 검증 범위

| 구분 | 기준 |
| :--- | :--- |
| 실행 | K8s `dev`/`test`/`qa` overlay |
| E2E | `test` overlay 2 replicas |
| HPA | `qa` overlay WebSocket HPA `1→2` 확장 중 담당 Pod 이전 |
| 관측 | Grafana 대시보드 |

### 7.2 보안 경계

`local-secret.yaml`은 로컬 dev/test/qa용 샘플 값만 담습니다. 실제 인증 정보는 Git에 올리지 않고 외부 Secret 저장소나 배포 파이프라인에서 주입해야 합니다.

현재 설계는 로컬에서 재현 가능한 실행과 검증에 초점을 둡니다. 실제 운영으로 확장하려면 멀티 노드 장애 실험, Secret·백업 관리, 롤링 업데이트 중 연결 유지 정책을 별도 운영 설계로 다뤄야 합니다.
