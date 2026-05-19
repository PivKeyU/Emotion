FROM golang:1.24-alpine AS builder

WORKDIR /src

# Pre-cache the module manifest. go.sum is optional — the repo may not ship one
# yet, and `COPY go.mod go.sum ./` would hard-fail in that case ("go.sum: not
# found"). Using `go.sum*` keeps the step working whether the file exists or
# not, and still gives us a cached `go mod download` layer when it does.
COPY go.mod go.sum* ./
RUN go mod download

# Build the binary. Reuse the same module + build caches so incremental
# rebuilds (only source changed) are fast.
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/emotion ./cmd/emotion

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata ffmpeg && \
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
