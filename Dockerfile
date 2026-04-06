# 1. 빌드 스테이지
FROM golang:1.26-alpine AS builder

WORKDIR /app

# [최적화 1] 필수 패키지만 설치
RUN apk add --no-cache git

# [최적화 2] 의존성 캐싱 (이 레이어는 go.mod가 변하지 않는 한 재실행되지 않음)
COPY go.mod go.sum ./
RUN go mod download

# [최적화 3] 빌드 인자를 '소스 코드 복사' 전으로 이동
# SERVICE_NAME이 바뀌면 소스 코드 복사부터 다시 하지만, 
# 동일 서비스의 코드만 바뀔 땐 go mod download 과정을 건너뜁니다.
ARG SERVICE_NAME

# 소스 코드 복사
COPY . .

# [최적화 4] 바이너리 다이어트 (-s -w)
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /app/server ./cmd/${SERVICE_NAME}

# [NEW] grpc_health_probe 다운로드 (Alpine 환경)
RUN wget -qO/bin/grpc_health_probe https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/v0.4.24/grpc_health_probe-linux-amd64 && \
    chmod +x /bin/grpc_health_probe

# 2. 실행 스테이지
FROM alpine:3.23

WORKDIR /app

# ca-certificates: HTTPS API 호출 시 필수
# tzdata: 한국 시간 설정을 위해 필수
RUN apk add --no-cache ca-certificates tzdata

# 빌더에서 생성된 정적 바이너리와 health_probe 복사
COPY --from=builder /app/server .
COPY --from=builder /bin/grpc_health_probe /bin/grpc_health_probe

# 컨테이너 실행 명령
CMD ["./server"]