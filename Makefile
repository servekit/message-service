.PHONY: all build test lint generate migrate fmt vet tidy run proto docker-up docker-down docker-logs

## build: Build server and migrate binaries
build:
	go build -o bin/server ./cmd/server/
	go build -o bin/migrate ./cmd/migrate/

## run: Run the server locally
run:
	go run ./cmd/server/

## test: Run tests with race detector
test:
	go test -race -coverprofile=coverage.out ./...

## lint: Run golangci-lint
lint:
	golangci-lint run ./...

## fmt: Format code
fmt:
	gofmt -w .
	goimports -w .

## vet: Run go vet
vet:
	go vet ./...

## generate: Run gorm.io/cli code generation
generate:
	gorm gen -i ./internal/store/models -o ./internal/store/generated

## migrate: Run database migrations (AutoMigrate)
migrate:
	go run ./cmd/migrate/

## proto: Generate protobuf code with buf
proto:
	buf generate

## tidy: Run go mod tidy
tidy:
	go mod tidy

## all: Format, vet, lint, test
all: fmt vet lint test

## docker-up: Build images and start postgres + message-service (waits for HTTP gateway)
docker-up:
	docker compose up --build -d
	@echo "waiting for HTTP gateway on :18082..."
	@for i in $$(seq 1 60); do \
		curl -sf -o /dev/null http://localhost:18082/v1/emails && echo "ready (took $${i}s)" && exit 0; \
		sleep 1; \
	done; \
	echo "timeout waiting for service"; docker compose logs --tail=30 message-service; exit 1

## docker-down: Stop containers and delete volumes (postgres data wiped)
docker-down:
	docker compose down -v

## docker-logs: Tail service logs (pass svc=name to filter)
docker-logs:
	docker compose logs -f --tail=100 $(svc)
