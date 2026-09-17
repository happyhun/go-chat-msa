# JetStream HPA 정합성 테스트 보고서

> 2026-09-16 KST, 기존 kind 클러스터의 `go-chat-qa` 환경에서 수행한 scale-up·강제 scale-in 및 저장 장애 복구 검증이다.

## 실행 조건

- 기존 dev 환경 제거: `make dev-down`. dev 네임스페이스와 MongoDB·NATS PVC를 삭제한 뒤 QA만 실행했다.
- QA 구성: `make qa-up`. 새 MongoDB·NATS 저장소, 마이그레이션, Prometheus custom metrics adapter를 포함한다.
- 테스트: `make qa-load K6_FOLLOW_LOGS=false`. k6 v1.5.0 Job 하나, 최대 300 VU, 30개 방, 5초 메시지 간격, 5분 부하 단계.
- k6는 클러스터 내부 `api-gateway:8080`, `websocket-service:8081` Service에 직접 연결한다. 별도로 실행한 Go E2E는 QA Ingress를 사용한다.
- WebSocket 송신 → JetStream 수락·RePublish → echo를 받은 메시지 ID를 MongoDB history 조회로 대조한다. 테스트 종료 뒤 새 QA MongoDB의 `content: "hpa-check"` 문서 수도 확인한다.
- HPA는 `gochat_ws_connections_active` 평균 100을 기준으로 WebSocket 파드를 1~2개 사이에서 조정한다.

## 결과

| 항목 | 관측값 |
| :--- | ---: |
| Job | Complete, 최대 300 VU, 완료 iteration 799 |
| WebSocket HPA | 1→2파드, 두 파드 Ready, 재시작 0 |
| 송신 시도 / echo 수신 | 11,985 / 11,985 |
| 새 QA MongoDB의 테스트 메시지 문서 | 11,985 |
| echo ID의 DB 미반영 | 0 |
| 송신 예외 / echo timeout | 0 / 0 |
| sync 오류 / 최종 누락 / 중복 실시간 전달 | 0 / 0 / 0 |
| 종료 뒤 JetStream persistence pending / DLQ | 0 / 0 |

테스트 스크립트는 계획된 90초 연결 종료와 같은 순간에 새 메시지를 보내지 않도록 종료 10초 전부터 송신을 멈춘다. echo 없이 종료된 송신은 timeout으로 집계해 조용히 통과하지 않도록 했다. `k6 inspect`는 실행 이미지인 `grafana/k6:1.5.0`에서 통과했다.

## 추가 장애 복구 검증

기존 E2E를 QA 환경에 연결해 한 번 실행했다. 각 단계에서 MongoDB를 중단하고 8건의 WebSocket echo 수락을 확인한 뒤 장애를 주입했다. 복구 뒤 누적 수락 ID 집합과 DB 이력을 정확히 대조하고 stream pending·DLQ가 0인지 확인했다. E2E 종료 시 기존 정리 절차가 QA 테스트 데이터를 삭제하고 원래 replica 수를 복원한다.

```sh
E2E_K8S_NAMESPACE=go-chat-qa \
E2E_GATEWAY_BASE_URL=http://qa.gochat.localhost:30080/api \
E2E_WS_BASE_URL=http://qa.gochat.localhost:30080 \
go test -count=1 -tags=e2e ./test/e2e \
  -run '^TestE2ESuite/TestScenario_17_DurableAcceptanceAndRestartRecovery$' -v -timeout=12m
```

| 단계 | 누적 수락·DB 건수 | 누락·중복·pending·DLQ | Mongo 복구 명령부터 대조 완료 |
| :--- | ---: | :--- | ---: |
| MongoDB 중단 후 복구 | 10 | 모두 0 | 7.734초 |
| MongoDB 중단 중 chat-service rollout restart | 18 | 모두 0 | 7.276초 |
| MongoDB 중단 중 chat-service 1→3→1 | 26 | 모두 0 | 7.182초 |
| MongoDB 중단 중 NATS Pod 교체, 같은 PVC UID 유지 | 34 | 모두 0 | 20.214초 |

worker 스케일 변경은 MongoDB 중단으로 backlog가 남아 있는 조건에서 검증했다. NATS 재시작 후에도 MongoDB 복구 전에 backlog가 남아 있음을 확인했다. 위 시간은 전체 장애 시간이 아닌 MongoDB 복구 명령 이후의 관측값이다.

직접 `PubAck`를 기록하는 기존 통합 테스트도 한 번 통과했다.

```sh
go test -count=1 -tags=integration ./internal/chat \
  -run '^TestJetStreamPersistenceRecovery$' -v -timeout=3m
```

이 테스트는 중복 재송신을 제외한 정상 메시지 52개의 수락 ID와 DB 집합을 대조하며, DB 저장 후 ACK하지 않은 메시지의 NATS 재시작 후 재전달·멱등 저장, scoped key 중복 제거, MongoDB 중단 중 circuit open과 복구를 검증한다. 의도적으로 보낸 malformed 메시지 한 건만 DLQ에 남고 정상 메시지의 pending은 0이었다. 사용한 임시 NATS·Mongo 컨테이너는 테스트 종료 시 제거됐다.

## 연결 유지 중 WebSocket scale-in

`make qa-load K6_FOLLOW_LOGS=false`를 한 번 실행했다. 1→2 확장 후 두 파드의 활성 연결 75·193개를 확인하고, 실행 약 2분 후 아래와 같이 HPA 상한을 낮춰 축소를 강제했다.

```sh
kubectl -n go-chat-qa patch hpa websocket-service --type=merge \
  -p '{"spec":{"maxReplicas":1,"behavior":{"scaleDown":{"stabilizationWindowSeconds":0}}}}'
```

HPA 이벤트에서 2→1 변경과 원인 `Current number of replicas above Spec.MaxReplicas`를 확인했다. 자연 부하 감소에 따른 축소가 아니라 활성 연결이 있는 조건의 강제 축소다.

| 항목 | 관측값 |
| :--- | ---: |
| Job | Complete, 최대 300 VU, 완료 iteration 885 |
| 송신 시도 / echo / MongoDB 테스트 문서 | 12,239 / 12,239 / 12,239 |
| 계획되지 않은 WebSocket 종료 | 99 |
| 재접속 / 재접속 직후 history 조회로 읽은 메시지 합계 | 585 / 4,468 |
| DB 미반영·timeout·sync 오류·최종 누락·중복 실시간 전달 | 모두 0 |
| MongoDB scoped key 중복 그룹 | 0 |
| 종료 뒤 stream pending / DLQ | 0 / 0 |

재접속·history 수치는 계획된 세션 교체를 포함하며 메시지 합계는 클라이언트 간 중복을 포함한다. 4,468건 모두가 scale-in으로 누락됐다는 뜻은 아니다. 종료 뒤 다음 명령으로 HPA를 원복했고 min=1, max=2, scale-down 안정화 300초를 확인했다.

```sh
kubectl -n go-chat-qa apply -f deploy/k8s/overlays/qa/apps/websocket-service-hpa.yaml
```

## 검증 상태와 해석 범위

`go test ./...`, 기본 `golangci-lint run`, 선택한 E2E·통합 테스트, k6 1.5.0의 `inspect`와 QA Job은 통과했다. 추가 E2E 태그 lint는 이번 변경 파일 밖의 기존 `errcheck` 26건·`unused` 2건으로 실패했다.

검증한 장애와 관측된 메시지 집합에서는 누락·논리 중복이 없었다. QA HPA는 echo 집합을 사용하며 전체 서버 `PubAck` 집합을 수집하지 않는다. 직접 PubAck 대조는 별도 통합 테스트의 52건 범위다. 장시간 장애, 네트워크 지연 주입, NATS 중단 중 신규 송신, 호스트·디스크 손실은 이번 실행으로 검증하지 않았다. C10K 지연 채택 기준은 별도 [성능 보고서](K8S_JETSTREAM_C10K_REPORT.md)의 결과를 따른다.
