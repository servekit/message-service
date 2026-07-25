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
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# Runtime stage:alpine has a shell so entrypoint.sh can run migrate first.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 appuser

WORKDIR /app
COPY --from=builder /out/server                              ./server
COPY --from=builder /out/migrate                             ./migrate
COPY --from=builder /src/message-service/entrypoint.sh       ./entrypoint.sh
COPY --from=builder /src/message-service/config.example.yaml  ./config.yaml
RUN chmod +x entrypoint.sh server migrate

USER appuser
EXPOSE 9000 8080
ENTRYPOINT ["./entrypoint.sh"]
