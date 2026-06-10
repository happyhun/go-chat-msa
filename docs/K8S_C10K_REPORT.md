# Kubernetes C10K 부하 테스트 보고서

> 이 문서는 로컬 kind Kubernetes `dev` 환경에서 확보한 C10K 성능 baseline입니다.

## 테스트 환경

| 항목 | 값 |
|------|-----|
| 일시 | 2026-06-10 KST |
| 명령 | `make kind-delete` → `make dev-up` → `make dev-load K6_FOLLOW_LOGS=false K6_LOAD_TIMEOUT=35m` |
| 도구 | k6 v1.5.0 / Kubernetes Job / 워커 4개 |
| 클러스터 | kind `go-chat`, Kubernetes v1.36.1 |
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
| msg_latency P99 | <50ms | 42ms | 41.84ms | 41ms | 43ms |
| history_fetch P99 | <100ms | 30.27ms | 29.82ms | 25.55ms | 22.87ms |
| sync_fetch P99 | <100ms | 30.45ms | 42.63ms | 27.39ms | 31.84ms |
| msg_timeouts | <1 | 0 | 0 | 0 | 0 |

---

## 서버

### Latency

| 지표 | 최대 P99 |
|------|----------|
| Fanout | 6.51ms |
| Egress | 24.2ms |

### CPU & 메모리

| 서비스 | CPU | 메모리 |
|--------|-----|--------|
| ws-gateway | 207% | 1,394 MiB |
| user-service | 183% | 44 MiB |
| websocket-service-1 | 97% | 312 MiB |
| websocket-service-2 | 97% | 314 MiB |
| mongo | 19% | 806 MiB |
| api-gateway | 6% | 123 MiB |
| chat-service | 6% | 38 MiB |
| postgres | 4% | 51 MiB |
| redis | 2% | 20 MiB |

---

## 분석

### ws-gateway — CPU 207%, 메모리 1,394 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | L7 프록시로 2K Ingress + 200K Egress 프레임 syscall 부하 |
| 메모리 | 고루틴 + 커널 TCP 버퍼가 연결 수에 비례해 양방향 누적 |

### user-service — CPU 183%, 메모리 44 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | ramp-up 구간에 10,000 VU의 회원가입(hash) + 로그인(compare) 동시 집중 |
| 메모리 | 무상태 서비스라 사용 적음 |

### websocket-service — 2대 합산 CPU 194%, 메모리 626 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | 2K Ingress + 200K Egress 프레임 syscall 부하 |
| 메모리 | 세션당 고루틴 스택 + 커널 TCP 버퍼가 연결 수에 비례해 누적 |

### mongo — CPU 19%, 메모리 806 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | 배치 저장 위주라 사용 적음 |
| 메모리 | 배치 삽입과 유니크 인덱스 유지로 인한 WiredTiger 캐시 |

---

## 주의사항

- k6 워커 분리
  - 단일 k6 프로세스의 JS 싱글스레드 병목을 피하기 위해 4개 워커로 분리
  - 워커당 2,500 VU로 클라이언트 병목 최소화
- 커널 튜닝
  - 공통: kind 컨테이너 런타임 기본 `nofile=1073741816`으로 충분해 별도 확장 불필요
  - 클라이언트: 임시 포트 고갈 방지 (`net.ipv4.ip_local_port_range=10240 65535`)
  - 서버: TCP Accept 큐 확장 (`net.core.somaxconn=65535`)
