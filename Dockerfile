FROM golang:1.24-alpine AS builder

WORKDIR /src

# Cache deps first.
COPY go.mod go.sum ./
RUN go mod download

# Build the binary.
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/emotion ./cmd/emotion

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 app

WORKDIR /app
COPY --from=builder /out/emotion /app/emotion
COPY --from=builder /src/.env.example /app/.env.example

USER app

EXPOSE 8096
ENV APP_NAME=emotion \
    SERVER_HOST=0.0.0.0 \
    SERVER_PORT=8096

ENTRYPOINT ["/app/emotion"]
