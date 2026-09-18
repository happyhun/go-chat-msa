# RFC-0003: WebSocket 큐 포화 시 연결 종료와 frame_no 제거

| 항목 | 내용 |
| :--- | :--- |
| 상태 | Experimenting |
| 작업 브랜치 | `dev` |
| 대상 | websocket-service, frontend, WebSocket 계약, 관측성, 테스트 |
| 기준 문서 | [DESIGN](../DESIGN.md), [RFC-0001](0001-stateless-websocket-broadcast.md), [RFC-0002](0002-jetstream-durable-message-persistence.md) |

## 제안과 구현

세션 전송 큐가 가득 차면 해당 세션을 닫힌 상태로 전환하고 WebSocket 소켓을 즉시 닫는다. 큐를 비우거나 close frame 쓰기가 끝나기를 기다리지 않는다. 클라이언트는 비정상 종료를 포함한 기존 onclose 경로에서 재연결하고 REST sync로 저장된 이력을 보충한다. 종료 후 신규 enqueue는 거절하고 기존 read/write loop와 Hub unregister 경로로 세션을 정리한다. 다른 세션의 fan-out은 계속한다.

전송 프레임의 `frame_no`, 세션별 번호와 payload 복사 버퍼, 프론트엔드 gap callback을 제거한다. Hub에서 공유한 불변 payload를 writer가 그대로 전송한다. 재연결·focus·수동·주기적 sync와 고정 recovery anchor는 유지한다. 짧은 연결이 반복되면 재시도 횟수와 backoff를 유지한다. 연결이 30초 이상 유지된 뒤 끊기면 재시도 횟수를 초기화하며, 명시적인 새 연결 요청도 횟수를 초기화한다.

`gochat_ws_send_queue_dropped_total`은 `gochat_ws_send_queue_overflows_total`로 바꾼다. 종료 메트릭은 최초 종료 상태 전환에서만 집계해 overflow와 방 종료가 겹쳐도 중복 집계하지 않는다. overflow는 세션당 최초 한 번 기록하며 `gochat_ws_sessions_closed_total`에도 `reason=send_queue_overflow`로 기록한다. Grafana와 텔레메트리 카탈로그를 함께 갱신한다. HPA 검증 스크립트에서 더 이상 제공하지 않는 frame gap 관측을 제거한다.

## 계약과 실패 모드

로컬 큐에서 메시지를 폐기한 뒤 세션을 정상 상태로 유지하지 않는다. 큐 포화 감지와 닫힌 상태 전환은 enqueue와 같은 잠금 안에서 처리한다. 종료 전 소켓에 이미 기록한 데이터는 클라이언트에 도착할 수 있지만 큐의 나머지 메시지 전달을 기다리지 않는다.

이 변경은 종단 간 무손실 전달을 보장하지 않는다. Core NATS에는 Pod별 ACK와 replay가 없고 MongoDB 반영은 비동기다. 복구는 기존의 제한된 기간 내 재조회 계약을 유지한다. 영속화하지 않는 시스템 이벤트는 REST 메시지 조회로 재생되지 않는다.

일시적인 큐 포화도 연결 종료와 티켓·sync 요청을 유발한다. 지속적으로 느린 클라이언트는 연결·종료를 반복할 수 있다. NATS write_deadline, NATS 오류 처리 범위, JSON v2 전환은 이번 변경 범위 밖이다.

기존 클라이언트는 frame_no가 없어도 메시지를 처리하며 종료 시 재연결한다. 새 클라이언트와 큐 포화 시 drop하는 구버전 서버의 혼용 기간에는 gap 기반 조기 복구를 기대할 수 없으므로 서버를 먼저 배포한다. 주기적 sync는 계속 유지한다.

## 검증

- 단위: 원본 payload 전송, 큐 포화 시 backlog 전송 없이 실제 연결 종료, 추가 프레임 없이 unregister, 동시 전송·종료, 정상 세션의 fan-out 유지, 실행 중인 writer의 쓰기가 막힌 상태에서 두 pump 종료와 unregister, overflow 직후 방 종료에도 종료 메트릭 단일 집계.
- 통합: NATS를 통한 echo·Pod 간 fan-out·시스템 프레임에서 frame_no 부재와 기존 연결 수명 검증.
- E2E: 실제 배포에서 frame_no 없는 채팅·시스템 프레임 처리, 재연결 후 REST 이력 복구, 기존 사용자 흐름과 저장 장애 복구.
- 정적 검사: go vet. 세션 동시성은 race detector로 확인한다.

큐 포화는 writer 시작 전 큐를 채우는 경우와 실행 중인 writer의 소켓 쓰기를 막는 경우로 나누어 결정적으로 재현한다. E2E는 느린 네트워크를 주입하는 부하 실험이 아니며 실제 overflow 유발까지 검증했다고 해석하지 않는다. 부하 테스트는 실행하지 않는다. 프론트엔드 테스트는 요청에 따라 추가하지 않으며 브라우저의 자동 재연결·backoff·sync 호출 연결은 직접 검증한 범위 밖이다.

현재 검증 결과:

| 검증 | 결과 |
| :--- | :--- |
| `go test ./...` | 통과 |
| `go vet ./...` | 통과 |
| `go test -race ./internal/websocket/hub` | 통과 |
| `go test -count=1 -tags=integration ./...` | 통과 |
| `make test-up` | 통과. 기존 Docker 빌드 단계의 프론트엔드 TypeScript·Vite 빌드 포함 |
| `go test -count=1 -tags=e2e ./test/e2e` | 통과, 144.528초 |
| HPA 스크립트 문법·대시보드 JSON | 통과 |

Graft 갱신은 로컬 Node 부재로 실행하지 못했으며, 기존 설치를 Node 컨테이너에서 실행하는 시도도 tree-sitter의 macOS/Linux 네이티브 모듈 불일치로 실패했다. 색인 파일을 수동으로 수정하지 않았다.

DESIGN과 그 다이어그램, Accepted RFC는 main의 기존 상태와 결정 기록으로 유지하고, 구현이 main에 반영된 뒤 DESIGN을 갱신한다.
