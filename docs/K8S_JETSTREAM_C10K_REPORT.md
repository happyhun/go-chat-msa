# JetStream C10K 부하 테스트 보고서

> 2026-09-16 KST 최종 실행. AC 전원·새 `go-chat-dev` 환경에서 최대 worker P99 50.00ms, 오류·timeout·OOM·재시작 0건. 엄격한 `<50ms` 기준은 충족하지 못했다.

## 조건과 측정 범위

- 실행: `make dev-load K6_FOLLOW_LOGS=false K6_LOAD_TIMEOUT=35m`, k6 v1.5.0 Kubernetes Job 4개 worker.
- 부하: worker당 2,500 VU, 총 목표 10,000 VU. 10분 ramp-up, 3분 plateau, 2분 ramp-down. 100개 방, VU당 5초 간격 128B 메시지.
- 경로: WebSocket 송신 → JetStream publish → stream RePublish → WebSocket echo. MongoDB는 durable consumer에서 비동기 배치 저장.
- k6는 클러스터 내부 `api-gateway:8080`, `websocket-service:8081` Service에 직접 연결한다. Ingress 구간은 측정 범위에 포함하지 않는다.
- `msg_latency`는 probe 역할 VU의 WebSocket 송신부터 자신의 echo까지의 표본이다. 모든 송신의 전수 전달·DB 저장 정합성은 이 테스트로 판정하지 않는다.
- 기준: 각 실행의 worker별 `msg_latency` P99가 모두 50ms 미만이고, 메시지 timeout·송신 오류 0건. [main 결과](K8S_C10K_REPORT.md)의 최대 P99는 43ms, [RFC-0001 결과](rfcs/0001-stateless-websocket-broadcast.md)의 최대 P99는 31ms다. 두 과거 결과는 비교 조건이 다른 참고치다.

## 결과

| Worker | P95 | P99 | `<50ms` 판정 |
| :--- | ---: | ---: | :--- |
| 1 | 14ms | 38.82ms | 통과 |
| 2 | 16ms | 36.83ms | 통과 |
| 3 | 15ms | 42.00ms | 통과 |
| 4 | 16ms | 50.00ms | 실패 |

### 재현 절차와 환경

기존 클러스터와 데이터 삭제를 승인받은 뒤 다음 순서로 한 번 실행했다. `kind-delete`는 기존 저장 데이터도 삭제하므로 재현 시 동일한 정리 범위를 확인해야 한다.

```sh
make kind-delete
make dev-up
make dev-load K6_FOLLOW_LOGS=false K6_LOAD_TIMEOUT=35m
```

kind 노드는 Kubernetes v1.37.0이며 WebSocket 2개, chat-service 1개, NATS 1개 구성이다. 부하 스크립트와 threshold는 변경하지 않았다. 실행 전후 `pmset -g batt`에서 AC Power를 확인했다.

- 실행 시간: 20:47:33~21:03:47 KST, worker별 측정 약 16분 2초.
- 각 worker 최대 2,500 VU, 활성 WebSocket 관측 최대 9,996개.
- 송신 시도 1,185,961건, probe echo 지연 표본 11,666개. worker별 P95는 14/16/15/16ms.
- 인증·입장·ticket·WebSocket 연결·송신 예외·메시지 timeout 모두 0건.
- history 조회 P99 최대 26.72ms, sync 조회 P99 최대 39.19ms.
- OOM·Pod 재시작 0건, worker 노드 메모리 표본 최대 약 7.9/15.7GiB. 종료 후 모든 서비스 Ready, `CHAT_PERSIST` 잔여 메시지와 DLQ 0건.
- worker 1~3은 모든 threshold를 통과했다. worker 4만 `msg_latency p(99)<50`에서 정확히 50ms로 실패해 exit code 99, Job 실패와 Make 오류로 종료됐다. 인프라 장애나 OOM이 아니다.

## 해석과 남은 검증

최대 worker P99는 50.00ms다. main 참고치 43ms 대비 +7.00ms(+16.3%), RFC-0001 참고치 31ms 대비 +19.00ms(+61.3%)다. RFC-0001과의 단순 수치 비교에서는 절대 15ms·상대 50% 이하 증가 기준을 넘으며, 독립적인 `<50ms` 기준도 통과하지 못했다. 결과에 맞춰 threshold를 완화하지 않는다.

기준선은 같은 날 동일 조건으로 수집한 대조군이 아니다. AC 전원·새 클러스터·초기화된 DB 조건의 단일 실행이며, 이 차이를 JetStream 단독 비용이나 충전기만의 효과로 단정하지 않는다. worker별 P99는 평균하거나 전체 요청의 통합 P99로 해석하지 않는다.

RFC-0001은 연결 후 되감기 재동기화를 측정 경로에서 제외했다. main은 ws-gateway 구조와 이전 Kubernetes·Go 버전에서 측정했으므로 세 결과를 동일 조건의 아키텍처 비교로 취급하지 않는다.

## 서버 egress

최종 실행의 `gochat_ws_egress_duration_seconds_bucket`을 Prometheus에서 조회했다. 서버가 메시지를 받은 시각부터 같은 발신자 ID 세션의 소켓 쓰기 직전까지 측정한다. 소켓 쓰기 완료와 클라이언트 수신·처리 시간은 포함하지 않는다. 같은 사용자가 여러 Pod에 연결되어 있으면 Pod 간 시계 차이가 섞일 수 있다. 아래 값은 histogram 보간 추정치다.

| 집계 범위 | P99 |
| :--- | ---: |
| 전체 실행, 두 WS Pod 합산 | 23.92ms |
| 최대 부하 유지 3분 | 26.94ms |
| 전체 실행 중 1분 창 P99 최댓값, 15초 간격 | 42.56ms |
| Pod별 전체 실행 | 23.84 / 23.99ms |

전체 실행은 20:47:33~21:03:47 KST의 bucket 증가량, 유지 구간은 21:00:40 KST 기준 직전 3분의 rate로 계산했다. 합산 P99의 PromQL은 `histogram_quantile(0.99, sum by (le) (increase(gochat_ws_egress_duration_seconds_bucket[16m14s])))`이며 평가 시각은 `2026-09-16T12:03:47Z`다.

main 보고서의 서버 egress 최대 P99는 24.2ms다. 수치상 전체 실행 23.92ms와 비슷하지만 main의 집계 구간과 측정 경계가 동일하다고 확인되지 않아 성능 동등성을 단정하지 않는다. Pod별 전체 P99는 비슷하며 k6 worker별 편차의 원인은 이 지표만으로 확정할 수 없다.

오류 카운터 0과 stream pending 0은 probe 외 전체 메시지의 무손실이나 MongoDB 최종 정합성을 증명하지 않는다. 별도로 완료한 HPA 정합성·장애 테스트는 [JetStream HPA 정합성 테스트 보고서](K8S_JETSTREAM_HPA_REPORT.md)에 기록한다.
