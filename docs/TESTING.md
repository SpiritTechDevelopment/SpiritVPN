# Тестирование SpiritVPN

Руководство по тестированию компонентов SpiritVPN.

## Типы тестов

### 1. Unit-тесты

Модульные тесты для изолированного тестирования функций и методов.

**Расположение:** `*_test.go` файлы рядом с исходным кодом

**Запуск:**
```bash
# Все unit-тесты
go test ./...

# Конкретный пакет
go test ./internal/vpn

# С покрытием
go test -cover ./...

# Детальный отчет о покрытии
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

**Планируемые тесты:**
- [TEST-01] `pkg/config/config_test.go` - тесты загрузки конфигурации
- [TEST-02] `internal/vpn/xray_test.go` - тесты XrayClient с моками gRPC

### 2. Smoke-тесты

Быстрые проверки основной функциональности после изменений.

**Расположение:** `test/smoke/`

#### Xray API Smoke Test

Проверяет работу Xray gRPC API (AddUser, GetStats, RemoveUser).

**Требования:**
- Запущенный VPN контейнер с Xray
- Доступ к API на `localhost:10085`

**Запуск:**
```bash
# Запустите VPN контейнер
docker compose up -d vpn

# Дождитесь запуска (5-10 секунд)
Start-Sleep -Seconds 5

# Запустите smoke тест
go run test/smoke/xray_test.go
```

**Ожидаемый вывод:**
```
=== SpiritVPN Xray API Smoke Test ===

1. Connecting to Xray API at 127.0.0.1:10085...
[OK] Connected successfully

2. Adding test user (UUID: 550e8400-e29b-41d4-a716-446655440000, Email: test-user@example.com)...
[OK] User added successfully

3. Getting stats for user test-user@example.com...
[OK] Stats retrieved: received=0 bytes, sent=0 bytes

4. Removing test user test-user@example.com...
[OK] User removed successfully

=== All tests passed! ===
```

### 3. Интеграционные тесты

Тестирование взаимодействия нескольких компонентов системы.

**Расположение:** `test/integration/`

**Статус:** Планируется

**Примеры:**
- Тесты API сервера с реальной БД
- Тесты Telegram бота с mock API
- End-to-end тесты пользовательских сценариев

### 4. E2E тесты

Полные сквозные тесты всей системы.

**Расположение:** `test/e2e/`

**Статус:** Планируется

**Примеры:**
- Регистрация → покупка подписки → подключение к VPN
- Продление подписки через Telegram бота
- Проверка работы VPN соединения

## Структура тестов

```
test/
├── smoke/              # Smoke-тесты основной функциональности
│   └── xray_test.go   # Проверка Xray API
├── integration/        # Интеграционные тесты (планируется)
└── e2e/               # End-to-end тесты (планируется)

internal/*/
└── *_test.go          # Unit-тесты рядом с кодом
```

## CI/CD

### GitHub Actions (планируется)

```yaml
- name: Run tests
  run: |
    go test -v -race -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out
```

### Pre-commit hooks

Рекомендуется настроить запуск тестов перед коммитом:

```bash
# .git/hooks/pre-commit
#!/bin/sh
go test ./...
```

## Тестирование с Docker

### Запуск всех smoke-тестов

```bash
# Пересобрать и запустить контейнеры
docker compose up -d --build

# Дождаться готовности
Start-Sleep -Seconds 10

# Smoke тесты
go run test/smoke/xray_test.go

# Остановить контейнеры
docker compose down
```

### Тестирование отдельного сервиса

```bash
# Только VPN сервер
docker compose up -d vpn
go run test/smoke/xray_test.go

# Только API сервер
docker compose up -d api db redis
# TODO: интеграционные тесты API
```

## Отладка тестов

### Просмотр логов контейнера

```bash
# Логи VPN сервера
docker compose logs vpn

# Логи в реальном времени
docker compose logs -f vpn
```

### Подключение к контейнеру

```bash
# Bash в VPN контейнере
docker compose exec vpn sh

# Проверка процессов
docker compose exec vpn ps aux

# Проверка конфигурации Xray
docker compose exec vpn cat /etc/xray/xray.json
```

### Тестирование Xray API вручную

```bash
# Проверка доступности API (внутри контейнера)
docker compose exec vpn nc -zv 127.0.0.1 10085

# Проверка с хоста (если порт проброшен)
Test-NetConnection -ComputerName localhost -Port 10085
```

## Best Practices

### 1. Изоляция тестов
- Каждый тест должен быть независимым
- Использовать `t.Cleanup()` для очистки ресурсов
- Избегать глобального состояния

### 2. Моки и заглушки
- Использовать `gomock` для gRPC клиентов
- Использовать `testify/mock` для интерфейсов
- Избегать реальных сетевых запросов в unit-тестах

### 3. Покрытие кода
- Целевое покрытие: >80%
- Критичные модули: >90% (vpn, payment, database)
- Проверять покрытие перед merge в main

### 4. Параллельное выполнение
```go
func TestSomething(t *testing.T) {
    t.Parallel() // Запуск теста параллельно
    // ...
}
```

### 5. Table-driven тесты
```go
func TestXrayClient_AddUser(t *testing.T) {
    tests := []struct {
        name    string
        uuid    string
        email   string
        wantErr bool
    }{
        {"valid user", "uuid-1", "user@example.com", false},
        {"invalid uuid", "invalid", "user@example.com", true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // ...
        })
    }
}
```

## Troubleshooting

### Тест не находит Xray API

```bash
# Проверьте, что контейнер запущен
docker compose ps

# Проверьте логи
docker compose logs vpn | Select-String "Xray"

# Убедитесь, что порт пробросан
docker compose port vpn 10085
```

### Connection refused

- Дождитесь полного запуска контейнера (5-10 секунд)
- Проверьте, что API слушает на `0.0.0.0:10085`, а не на `127.0.0.1`
- Убедитесь, что файрволл не блокирует порт

### Тесты проходят локально, но падают в CI

- Проверьте версию Go в CI (требуется 1.23+)
- Убедитесь, что все зависимости установлены
- Проверьте таймауты (CI может быть медленнее)

## Roadmap тестирования

- [x] Smoke-тест Xray API
- [ ] [TEST-01] Unit-тесты для pkg/config
- [ ] [TEST-02] Unit-тесты для internal/vpn с моками
- [ ] Интеграционные тесты API сервера
- [ ] Интеграционные тесты Telegram бота
- [ ] E2E тесты пользовательских сценариев
- [ ] Автоматизация в GitHub Actions
- [ ] Benchmark тесты производительности VPN
- [ ] Stress тесты под нагрузкой

## Ссылки

- [Go Testing Package](https://pkg.go.dev/testing)
- [testify](https://github.com/stretchr/testify)
- [gomock](https://github.com/golang/mock)
- [Table Driven Tests](https://go.dev/wiki/TableDrivenTests)
