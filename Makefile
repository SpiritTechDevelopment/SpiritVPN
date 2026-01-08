# Makefile for SpiritVPN

.PHONY: help build run test clean docker deps

help: ## Показать помощь
	@echo "Доступные команды:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-15s\033[0m %s\n", $$1, $$2}'

deps: ## Установить зависимости
	go mod download
	go mod tidy

build: ## Собрать все сервисы
	@echo "Building API Server..."
	go build -o bin/api-server ./cmd/api-server
	@echo "Building VPN Server..."
	go build -o bin/vpn-server ./cmd/vpn-server
	@echo "Building Telegram Bot..."
	go build -o bin/telegram-bot ./cmd/telegram-bot
	@echo "Build complete!"

run-api: ## Запустить API сервер
	go run ./cmd/api-server/main.go

run-vpn: ## Запустить VPN сервер
	go run ./cmd/vpn-server/main.go

run-bot: ## Запустить Telegram бота
	go run ./cmd/telegram-bot/main.go

test: ## Запустить тесты
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

test-coverage: ## Запустить тесты и показать покрытие
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out
	@COVERAGE=$$(go tool cover -func=coverage.out | grep total | awk '{print $$3}'); \
	echo ""; \
	echo "Total coverage: $$COVERAGE"

test-coverage-html: ## Запустить тесты и открыть HTML отчет
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out

test-unit: ## Запустить только unit тесты
	go test -v -short ./...

test-integration: ## Запустить только интеграционные тесты
	go test -v -run Integration ./...

lint: ## Запустить линтер
	golangci-lint run

hooks: ## Установить git hooks (требуется pre-commit)
	pre-commit install

clean: ## Очистить build артефакты
	rm -rf bin/
	rm -f coverage.out coverage.html

docker-build: ## Собрать Docker образы
	docker-compose build

docker-up: ## Запустить все сервисы в Docker
	docker-compose up -d

docker-down: ## Остановить все сервисы
	docker-compose down

docker-logs: ## Показать логи
	docker-compose logs -f

migrate-up: ## Запустить миграции
	@echo "Running migrations..."
	# TODO: добавить команду миграции

migrate-down: ## Откатить миграции
	@echo "Rolling back migrations..."
	# TODO: добавить команду отката

dev: ## Запустить в режиме разработки
	@echo "Starting development environment..."
	docker-compose up -d postgres redis
	go run ./cmd/api-server/main.go
