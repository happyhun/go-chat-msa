# C10K 트러블슈팅 저널

k6로 C10K 부하테스트를 진행하며 겪은 문제들과 해결 과정을 기록했습니다.

---

## Case 1: Panic on Closed Channel

### 증상
`panic: send on closed channel`로 서버 비정상 종료

### 원인
- Hub가 세션을 종료하며 채널을 닫는 시점과, ReadPump가 메시지를 전송하려는 시점이 충돌

### 해결
- 채널 닫기를 뮤텍스로 보호하고, 전송 측에서 `closed` 플래그를 먼저 확인하여 닫힌 채널에 쓰는 상황을 원천 차단

### 결과
- panic 해소

---

## Case 2: Death Spiral

### 증상
10K 유저 가입 및 로그인 시:
- HTTP 실패율: 99.55%
- HTTP P99 레이턴시: 타임아웃 초과(>10s)
- 무한 재시도로 로그인 400,000회 이상 폭증

### 원인
- `bcrypt` 해싱(Cost 10)에 대해 무제한 고루틴이 생성되어 CPU 경합 발생
- 서버 부하로 타임아웃 발생 시 클라이언트가 즉시 재시도

### 해결
- 동시 해싱 작업 수를 CPU 코어 수에 맞게 제한하는 워커 풀 도입
- 큐가 가득 차면 즉시 `ErrQueueFull`을 반환하여 연쇄 장애 차단
- 테스트 스크립트에 Jitter 포함 백오프를 추가하여 재시도 부하 분산

### 결과
- HTTP 실패율: 99.55% → 81%
- HTTP P99 레이턴시: 10s+ → 5.32s
- CPU 경합은 감소하였으나 여전히 레이턴시가 높아 Ramp-up 시간 10분으로 증대

---

## Case 3: Port Exhaustion

### 증상
- `dial tcp ... connect: cannot assign requested address` 에러 발생

### 원인
- k6 클라이언트에서 대량 연결 시 `TIME_WAIT` 상태의 로컬 포트가 누적되어 가용 포트 고갈

### 해결
- `ip_local_port_range`를 `10240~65535`로 확장하여 아웃바운드 포트 수용량 확보

### 결과
- 포트 고갈 해소

---

## Case 4: Client-Side Measurement Bottleneck

### 증상
- k6 측 메시지 레이턴시가 10초 초과

### 원인
- 단일 k6 프로세스가 초당 200K의 수신 메시지를 JSON 파싱하면서 JS 싱글스레드 CPU 포화
- 메시지 수신 처리가 밀리면서 타임스탬프 측정 시점이 지연되어 레이턴시가 실제보다 높게 측정됨

### 해결
- k6 워커를 4개로 분리하여 프로세스당 부하를 1/4로 분산
- 전체 VU의 1%만 메시지 파싱을 수행하는 Probe 패턴 도입으로 클라이언트 CPU 보호

### 결과
- 클라이언트 병목 해소로 서버 측과 유사한 레이턴시 확보

---

## Case 5: Per-Message Persistence Bottleneck

### 증상
- 드랍이나 에러 없이 레이턴시만 비정상적으로 높음
- 서버 Egress P99 1초, k6 msg_latency P99 2초


### 원인
- 메시지마다 개별 gRPC 호출로 저장 (초당 2,000건 = 초당 2,000회 gRPC 라운드트립 + MongoDB 쓰기)
- 단일 호스트에서 저장 gRPC 호출의 커널 컨텍스트 스위칭이 WebSocket 통신과 경합하여 전체 통신 지연

### 해결
- 배치 파이프라인 도입: 고정 워커가 500건 단위로 모아 `BatchCreateMessages` 호출. 100ms 타이머로 주기적 플러시
- 저장 실패 시 재시도 큐에서 지터 포함 지수 백오프로 격리 처리

### 결과
- 서버 Egress P99: 1초 → 5ms
- k6 msg_latency P99: 2초 → 25ms
