FROM golang:1.24 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /out/upupup-server ./cmd/upupup-server

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /out/upupup-server /usr/local/bin/upupup-server

ENTRYPOINT ["/usr/local/bin/upupup-server", "--config", "/app/config.yml"]


