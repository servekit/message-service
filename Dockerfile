# Build stage.
# Build context MUST be the monorepo root (parent of message-service/) so that
# go.mod replace directives (../go-common, ../gid-service) resolve correctly
# inside the container. With docker-compose, set:
#   build:
#     context: ..
#     dockerfile: message-service/Dockerfile
FROM golang:1.26-alpine AS builder
WORKDIR /src

# Copy entire monorepo so local replace paths resolve.
COPY . .

WORKDIR /src/message-service
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/message-service ./cmd/server

# Runtime stage.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 appuser

WORKDIR /app
COPY --from=builder /out/message-service                       ./message-service
COPY --from=builder /src/message-service/config.example.yaml   ./config.yaml
RUN chmod +x message-service

USER appuser
EXPOSE 19092 18082

# Args pass through to the binary:
#   docker run <image>            -> ./message-service         (start server)
#   docker run <image> migrate    -> ./message-service migrate (run migrations)
ENTRYPOINT ["./message-service"]
