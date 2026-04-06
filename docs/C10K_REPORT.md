# C10K 부하 테스트 보고서

## 테스트 환경

| 항목 | 값 |
|------|-----|
| 도구 | k6 v1.5.0 / 워커 4대 / Docker Compose |
| 호스트 | MacBook Pro M4 Pro, 12코어, 24GB RAM |
| 런타임 | OrbStack (linux/arm64) |

### 부하 프로필

| 항목 | 값 |
|------|-----|
| VU | 10,000 (워커당 2,500) |
| 단계 | ramp-up 10m → steady 3m → ramp-down 2m |
| 역할 | Stalker 90% / Reconnector 7% / Churner 2% / Probe 1% |
| 메시지 | 5초 간격, 128B 페이로드 |
| 채팅방 | 100개 (방당 100명) |
| Ingress RPS | 2K |
| Egress RPS | 200K |

---

## 클라이언트

4개 워커 모두 PASS

| 메트릭 | 임계값 | W1 | W2 | W3 | W4 |
|--------|--------|----|----|----|----|
| msg_latency P99 | <50ms | 21ms | 21ms | 19ms | 25ms |
| history_fetch P99 | <100ms | 11ms | 11ms | 10ms | 12ms |
| sync_fetch P99 | <100ms | 19ms | 15ms | 15ms | 17ms |
| msg_timeouts | <1 | 0 | 0 | 0 | 0 |

---

## 서버

### Latency

| 지표 | P99 |
|------|-----|
| Fanout | 2.0ms |
| Egress | 5.0ms |

### CPU & 메모리

| 서비스 | CPU | 메모리 |
|--------|-----|--------|
| ws-gateway | 229% | 1,363 MiB |
| user-service | 178% | 20 MiB |
| websocket-service-1 | 97% | 283 MiB |
| websocket-service-2 | 110% | 318 MiB |
| mongo | 12% | 619 MiB |
| chat-service | 6% | 21 MiB |
| api-gateway | 4% | 115 MiB |
| postgres | 3% | 20 MiB |

---

## 분석

### ws-gateway — CPU 229%, 메모리 1,363 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | L7 프록시로 2K Ingress + 200K Egress 프레임 syscall 부하 |
| 메모리 | 고루틴 + 커널 TCP 버퍼가 연결 수에 비례해 양방향 누적 |

### user-service — CPU 178%, 메모리 20 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | ramp-up 구간에 10,000 VU의 회원가입(hash) + 로그인(compare) 동시 집중 |
| 메모리 | 무상태 서비스라 사용 적음 |

### websocket-service — 2대 합산 CPU 207%, 메모리 601 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | 2K Ingress + 200K Egress 프레임 syscall 부하 |
| 메모리 | 세션당 고루틴 스택 + 커널 TCP 버퍼가 연결 수에 비례해 누적 |

### mongo — CPU 12%, 메모리 619 MiB

| 리소스 | 원인 |
|:---|:---|
| CPU | 배치 저장 위주라 사용 적음 |
| 메모리 | 배치 삽입과 유니크 인덱스 유지로 인한 WiredTiger 캐시 |

---

## 주의사항

- k6 워커 분리
  - 단일 k6 프로세스 사용 시 JS 싱글스레드 병목 발생
  - 4개 워커로 분리하여 클라이언트 병목 최소화
- 커널 튜닝
  - 공통: 파일 디스크립터 한도 확장 (`ulimit nofile=65535`)
  - 클라이언트: 임시 포트 고갈 방지 (`net.ipv4.ip_local_port_range=10240 65535`)
  - 서버: TCP Accept 큐 확장 (`net.core.somaxconn=65535`)
