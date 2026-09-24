FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
ARG SERVICE_NAME
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app/server ./cmd/${SERVICE_NAME}

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 app \
    && adduser -D -H -u 10001 -G app app
WORKDIR /app
COPY --from=builder /app/server .
USER 10001:10001
CMD ["./server"]
