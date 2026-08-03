FROM golang:1.22-alpine AS builder

WORKDIR /app

# 设置 Go 代理
ENV GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o opencode2api main.go

FROM alpine:latest

RUN apk add --no-cache ca-certificates bash curl

WORKDIR /app

COPY --from=builder /app/opencode2api .
COPY --from=builder /app/config.yaml.example ./config.yaml.example

EXPOSE 8080

ENTRYPOINT ["./opencode2api"]
