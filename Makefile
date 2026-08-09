# Makefile for SpiritVPN backend

.PHONY: help build test test-coverage test-unit test-integration lint clean deps \
        proto proto-lint proto-breaking proto-format sqlc sqlc-vet \
        migrate-up migrate-down migrate-version \
        docker-build docker-up docker-down docker-logs dev dev-db dev-db-down hooks

# Подключение к эфемерной dev-базе из docker-compose.dev.yml. Переопределяется из
# окружения: DATABASE_URL=... make test-integration.
DEV_DATABASE_URL ?= postgres://spiritdb:spiritdb@localhost:5433/spiritdb?sslmode=disable

help: ## Показать помощь
	@echo "Доступные команды:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'

deps: ## Установить зависимости
	go mod download
	go mod tidy

# --- Protobuf -------------------------------------------------------------
# buf и плагины protoc-gen-* запиннены как go tool-зависимости (блок `tool` в
# go.mod) и вызываются через `go tool`, поэтому у всех одна версия.

proto: ## Сгенерировать Go-код из proto (buf, managed mode -> internal/gen)
	go tool buf generate

proto-lint: ## Проверить proto линтером buf
	go tool buf lint

proto-breaking: ## Проверить proto на несовместимые изменения относительно main
	go tool buf breaking --against '.git#branch=main'

proto-format: ## Отформатировать наши proto in-place (кроме vendored nodeagent)
	go tool buf format -w proto/spiritvpn/customer/v1/customer.proto
	go tool buf format -w proto/spiritvpn/manifest/v1/manifest.proto

# --- SQL ------------------------------------------------------------------
# sqlc читает схему из internal/migrations и генерит типобезопасный код запросов
# в internal/postgres/db. Схема-истина одна и та же для базы и для генерации.

sqlc: ## Сгенерировать Go-код запросов из SQL (sqlc)
	go tool sqlc generate

sqlc-vet: ## Проверить SQL-запросы по схеме без генерации
	go tool sqlc vet

# --- Build ----------------------------------------------------------------

build: ## Собрать бинарники
	go build -o bin/migrate ./cmd/migrate

# --- Migrations -----------------------------------------------------------

migrate-up: ## Накатить все ожидающие миграции
	go run ./cmd/migrate up

migrate-down: ## Откатить одну последнюю миграцию
	go run ./cmd/migrate down

migrate-version: ## Показать текущую версию схемы
	go run ./cmd/migrate version

# --- Test / lint ----------------------------------------------------------

test: ## Запустить тесты
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

test-coverage: ## Запустить тесты и показать покрытие
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out
	@COVERAGE=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}'); \
	echo ""; \
	echo "Total coverage: $$COVERAGE"

test-unit: ## Запустить только unit тесты
	go test -v -short ./...

test-integration: ## Запустить интеграционные тесты (сначала `make dev-db`)
	SPIRITVPN_INTEGRATION_TESTS=1 DATABASE_URL='$(DEV_DATABASE_URL)' \
		go test -v -count=1 -run Integration ./...

lint: ## Запустить линтер
	golangci-lint run

hooks: ## Установить git hooks (требуется pre-commit)
	pre-commit install

clean: ## Очистить build артефакты
	rm -rf bin/
	rm -f coverage.out coverage.html

# --- Docker / dev ---------------------------------------------------------

docker-build: ## Собрать Docker образы
	docker-compose build

docker-up: ## Запустить сервисы в Docker
	docker-compose up -d

docker-down: ## Остановить сервисы
	docker-compose down

docker-logs: ## Показать логи
	docker-compose logs -f

dev-db: ## Поднять эфемерный PostgreSQL для тестов (tmpfs, порт 5433)
	docker compose -f docker-compose.dev.yml up -d --wait

dev-db-down: ## Остановить эфемерный PostgreSQL и удалить его данные
	docker compose -f docker-compose.dev.yml down -v

dev: dev-db ## Поднять dev-базу и накатить на неё миграции
	DATABASE_URL='$(DEV_DATABASE_URL)' go run ./cmd/migrate up
