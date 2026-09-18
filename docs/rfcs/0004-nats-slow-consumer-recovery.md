# RFC-0004: NATS slow consumer 연결 재설정과 복구 순서

| 항목 | 내용 |
| :--- | :--- |
| 상태 | Experimenting |
| 작업 브랜치 | `dev` |
| 대상 | websocket-service, NATS 설정, 테스트 |
| 기준 | [DESIGN](../DESIGN.md), [RFC-0003](0003-websocket-overflow-disconnect.md) |

## 제안과 구현

NATS 클라이언트가 `ErrSlowConsumer`를 보고하면 `ForceReconnect`로 해당 Pod의 NATS 연결을 재설정한다. 메시지 구독과 `room.event.*` 구독에 같은 정책을 적용한다. 기존 disconnect handler를 통해 Pod의 모든 WebSocket 세션을 종료하고, 클라이언트의 reconnect → sync 복구 경로로 합류한다. WebSocket 개별 send queue overflow는 해당 세션만 종료한다.

NATS 오류 콜백은 직렬 처리된다. 연결 재설정 시작부터 reconnect callback까지 재설정 상태를 유지해, 먼저 대기 중이던 여러 구독의 오류가 중복 재접속을 유발하지 않도록 한다. `ForceReconnect`를 재연결 중 반복 호출하면 기존 backoff를 건너뛸 수 있으므로 이를 방지한다.

재설정 중에는 Bus의 연결 상태를 false로 노출한다. Manager는 NATS 연결 상태와 세션 정리 완료를 모두 확인한다. Hub의 종료 완료는 모든 세션의 read/write 루프가 끝난 뒤 알리고, Manager는 기존 Hub와 구독을 제거한 뒤 연결 수용을 재개한다. 장애 세대 확인과 readiness 복구 상태 변경은 같은 잠금으로 보호한다. 등록 준비와 commit도 연결 상태를 확인한다.

NATS 서버의 `write_deadline`을 10초에서 3초로 줄인다. 이는 서버가 클라이언트 소켓에 쓰기를 완료하지 못하는 경우의 제한이며, 구독 콜백 처리 지연이나 메시지 전체 전달 지연의 상한이 아니다. Go 클라이언트의 구독 버퍼 포화는 별도로 오류 콜백에서 처리한다. 기존 배포는 ConfigMap 갱신만으로 실행 중 서버 설정이 바뀌지 않으므로 NATS Pod 재시작 후 적용을 확인한다.

## 실패 모드와 트레이드오프

한 NATS 연결의 구독 손실은 해당 Pod 전체 세션 종료로 복구한다. 일시적 과부하에도 재접속과 sync 요청이 증가하며, 부하가 지속되면 반복될 수 있다. 다른 Pod의 연결은 이 오류 처리로 닫지 않는다.

오류 콜백은 첫 메시지가 이미 버려진 뒤 실행되는 비동기 알림이다. 손실 감지와 WebSocket 종료 사이에는 콜백 대기 및 기존 종료 지연이 존재한다. 따라서 한 건도 drop하지 않는 전달 보장이 아니라, 손실을 감지한 연결을 계속 정상으로 취급하지 않는 정책이다. Core NATS의 비영속 시스템 이벤트는 REST 메시지 sync로 재생되지 않는다.

`/ready`는 애플리케이션 연결 상태를 반영한다. Kubernetes는 readiness probe로 이를 관찰한다. 현재 probe는 5초 주기, 실패 임계값 6회이므로 빠르게 복구되면 Pod가 NotReady로 전환되지 않을 수 있다. 다른 Pod로의 재분배를 보장하지 않으며 같은 Pod로 재연결해도 기존 세션 정리 후 정상 처리한다.

기존 NATS disconnect의 세션 종료 지연 정책은 유지한다. 세션 종료 완료 대기는 처리 중인 publish의 취소와 WebSocket 쓰기 제한에 의존한다. NATS 연결만 빨리 복구됐다는 이유로 기존 세션이 남은 상태에서 readiness를 복구하지 않는다.

## 검증

- 단위: 두 방의 실제 WebSocket과 준비 중 등록을 둔 상태에서 disconnect/reconnect를 연속 호출한다. 처리 중인 세션 루프를 막아 readiness가 계속 false인지, 신규 등록과 commit이 거절되는지, 모든 기존 Hub 종료 후에만 readiness와 새 Hub 생성이 복구되는지 검증한다.
- 통합: 실제 NATS 컨테이너에서 메시지 구독과 와일드카드 이벤트 구독을 각각 포화시킨다. 두 구독의 오류를 대기시킨 후 연결 재설정이 한 번만 발생하고 재연결 후 구독 수신이 복구되는지 검증한다.
- E2E: NATS 재시작 시 기존 WebSocket의 1012 종료, 재접속, 저장 이력 복구와 재연결 후 실시간 수신을 검증한다. 실제 slow consumer 포화 주입은 통합 테스트 범위다.
- 프론트엔드 테스트와 부하 테스트는 수행하지 않는다. 3초 deadline의 운영 부하 적합성은 이번 기능 검증 범위 밖이다.

검증 결과:

| 검증 | 결과 |
| :--- | :--- |
| `go test ./...` | 통과 |
| `go vet ./...` | 통과 |
| `go test -race ./internal/websocket/hub` | 통과 |
| `go test -count=1 -tags=integration ./...` | 통과 |
| NATS slow consumer 통합 테스트의 race 검사 | 통과 |
| `make k8s-validate` | 통과 |
| `make test-up` | 통과 |
| `go test -count=1 -tags=e2e ./test/e2e` | 통과, 149.790초 |
| 재시작 후 NATS `/varz`의 `write_deadline` | `3000000000`ns, 3초 적용 확인 |

Graft 갱신은 RFC-0003에 기록한 실행 환경 문제로 수행하지 못했다. DESIGN과 Accepted RFC는 변경하지 않는다.
