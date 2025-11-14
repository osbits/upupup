FROM golang:1.24 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /out/monitor ./cmd/monitor

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /out/monitor /usr/local/bin/monitor
COPY config.yml /app/config.yml

ENV MONITOR_CONFIG=/app/config.yml

ENTRYPOINT ["/usr/local/bin/monitor","-config","/app/config.yml"]

