# Kubernetes NATS C10K 부하 테스트 보고서

> 이 문서는 로컬 kind `dev` 환경에서 확보한 NATS 구성의 C10K 성능 기준입니다.

## 테스트 환경

| 항목 | 값 |
|------|-----|
| 일시 | 2026-09-24 KST |
| 명령 | `make dev-load` (클러스터·DB 초기화 후 실행) |
| 도구 | k6 v2.3.0 / Kubernetes Job / 워커 4개 |
| 클러스터 | kind `go-chat`, Kubernetes v1.37.0 |
| 노드 | control-plane 1개, worker 1개 |
| 런타임 | OrbStack, linux/arm64 |
| Namespace | `go-chat-dev` |

### 부하 프로필

| 항목 | 값 |
|------|-----|
| VU | 10,000 (워커당 2,500) |
| 단계 | ramp-up 10m → steady 3m → ramp-down 2m |
| 역할 | Stalker 90% / Reconnector 7% / Churner 2% / Probe 1% |
| 메시지 | 5초 간격, 128B 페이로드 |
| 채팅방 | 100개 (방당 100명) |
| Ingress RPS | 약 2K |
| Egress RPS | 약 200K |

---

## 클라이언트

4개 워커 모두 PASS

| 메트릭 | 임계값 | W1 | W2 | W3 | W4 |
|--------|--------|----|----|----|----|
| msg_latency P99 | <50ms | 19ms | 16ms | 12ms | 12ms |
| history_fetch P99 | <100ms | 9.95ms | 11.09ms | 9.90ms | 10.61ms |
| sync_fetch P99 | <100ms | 16.05ms | 13.90ms | 13.47ms | 13.10ms |
| msg_timeouts | <1 | 0 | 0 | 0 | 0 |

---

## 서버

### Latency

| 지표 | 최대 P99 |
|------|----------|
| Fanout | 1.53ms |
| Egress | 8.86ms |

### CPU & 메모리

| 서비스 | CPU | 메모리 |
|--------|-----|--------|
| nats | 16% | 258 MiB |
| user-service | 217% | 38 MiB |
| websocket-service-1 | 103% | 413 MiB |
| websocket-service-2 | 105% | 427 MiB |
| mongo | 14% | 637 MiB |
| api-gateway | 6% | 115 MiB |
| chat-service | 7% | 38 MiB |
| postgres | 3% | 45 MiB |
| redis | 2% | 20 MiB |

---

## 분석

### nats — CPU 16%, 메모리 258 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | 발행·RePublish·consumer 전달 처리에 CPU를 사용하나, 이번 부하에서는 사용량이 낮음 |
| 메모리 | 메시지 버퍼와 스트림·consumer 상태 유지에 사용 |

### user-service — CPU 217%, 메모리 38 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | CPU 사용 중심: ramp-up 구간의 회원가입(hash) + 로그인(compare) 연산 집중 |
| 메모리 | 장기 연결이나 메시지를 보관하지 않는 무상태 서비스로 사용량이 작음 |

### websocket-service — 2대 합산 CPU 208%, 메모리 836 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | CPU·메모리 모두 사용: 약 2K Ingress + 200K Egress 메시지 처리와 소켓 송수신 |
| 메모리 | 1만 연결의 세션 상태·고루틴 스택·송수신 버퍼 유지 |

### mongo — CPU 14%, 메모리 637 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | 배치 저장과 이력·재동기화 조회 처리에 사용하며, 이번 부하에서는 사용량이 낮음 |
| 메모리 | 메모리 사용 중심: 메시지 데이터·인덱스 접근을 위한 WiredTiger 캐시 유지 |

---

## 주의사항

- k6 워커 분리
  - 4개 워커로 부하 분산
  - 워커당 2,500 VU, Probe 1%의 echo 지연 측정
- 커널 튜닝
  - 클라이언트: k6 Pod의 임시 포트 범위 확장 (`net.ipv4.ip_local_port_range=10240 65535`)
  - 서버: api-gateway·user-service·chat-service·websocket-service Pod의 TCP Accept 큐 확장 (`net.core.somaxconn=65535`)
