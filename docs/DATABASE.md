# Database Architecture

## Модели данных

Проект использует GORM для работы с PostgreSQL. Все модели находятся в `internal/database/models.go`.

### Основные модели:

1. **User** - пользователи системы, идентификация через Telegram ID
2. **Subscription** - подписки пользователей с информацией о тарифных планах и сроках
3. **VPNConfig** - VPN конфигурации с VLESS UUID и настройками
4. **VPNServer** - VPN серверы в различных географических локациях
5. **Payment** - платежи и транзакции пользователей
6. **TrafficStat** - ежедневная статистика использования трафика
7. **SubscriptionPlan** - тарифные планы с ценами и ограничениями

## Связи между таблицами

```
User (1) ──< (N) Subscription
User (1) ──< (N) VPNConfig
User (1) ──< (N) Payment
User (1) ──< (N) TrafficStat

Subscription (1) ──< (N) VPNConfig
Subscription (1) ──< (N) Payment

VPNServer (1) ──< (N) VPNConfig

VPNConfig (1) ──< (N) TrafficStat
```

## Использование Repository

Repository слой предоставляет абстракцию над GORM для работы с моделями данных.
Каждая модель имеет свой репозиторий с методами CRUD и специализированными запросами.

### Пример создания пользователя:

```go
// Создание репозитория
userRepo := database.NewUserRepository(db)

// Создание пользователя
user := &database.User{
    TelegramID: 123456789,
    Username:   "john_doe",
    Email:      "john@example.com",
}

err := userRepo.Create(user)
if err != nil {
    log.Fatal(err)
}

// Поиск пользователя
foundUser, err := userRepo.GetByTelegramID(123456789)
```

### Пример работы с подписками:

```go
subscriptionRepo := database.NewSubscriptionRepository(db)

// Создание подписки
subscription := &database.Subscription{
    UserID:    user.ID,
    PlanType:  "premium",
    StartDate: time.Now(),
    EndDate:   time.Now().AddDate(0, 1, 0), // +1 месяц
    IsActive:  true,
    AutoRenew: true,
}

err := subscriptionRepo.Create(subscription)

// Получение активной подписки
activeSub, err := subscriptionRepo.GetActiveByUserID(user.ID)

// Проверка истечения
if activeSub.IsExpired() {
    fmt.Println("Подписка истекла!")
}

fmt.Printf("Осталось дней: %d\n", activeSub.DaysLeft())
```

### Пример работы с VPN конфигурациями:

```go
configRepo := database.NewVPNConfigRepository(db)
serverRepo := database.NewVPNServerRepository(db)

// Получение оптимального сервера
server, err := serverRepo.GetOptimal()
if err != nil {
    log.Fatal(err)
}

// Создание конфигурации
config := &database.VPNConfig{
    UserID:         user.ID,
    SubscriptionID: subscription.ID,
    ServerID:       server.ID,
    UUID:           "generated_uuid",
    Flow:           "xtls-rprx-vision",
}

err = configRepo.Create(config)

// Увеличение счетчика пользователей на сервере
err = serverRepo.IncrementUsers(server.ID)
```

### Пример работы с платежами:

```go
paymentRepo := database.NewPaymentRepository(db)

// Создание платежа
payment := &database.Payment{
    UserID:         user.ID,
    SubscriptionID: &subscription.ID,
    Amount:         599.00,
    Currency:       "RUB",
    Status:         "pending",
    PaymentMethod:  "yookassa",
    TransactionID:  "unique_transaction_id",
}

err := paymentRepo.Create(payment)

// Обновление статуса после webhook
err = paymentRepo.UpdateStatus(payment.ID, "succeeded")

// Проверка статуса
if payment.IsSuccessful() {
    // Активировать подписку
}
```

## Миграции

Миграции выполняются автоматически при запуске приложения через GORM AutoMigrate.
Система миграций обеспечивает актуальность структуры базы данных без ручного вмешательства.

```go
db, err := database.Connect(cfg)
if err != nil {
    log.Fatal(err)
}

// Выполнение миграций
err = database.Migrate(db)
if err != nil {
    log.Fatal(err)
}
```

AutoMigrate:
- Создает таблицы если их нет
- Добавляет недостающие столбцы
- Создает индексы
- **НЕ удаляет** столбцы (для безопасности данных)

## Индексы

Созданы следующие индексы для оптимизации:

- `users.telegram_id` - уникальный индекс
- `subscriptions(user_id, is_active)` - для поиска активных подписок
- `payments(status, created_at)` - для фильтрации платежей
- `vpn_servers(is_active, load_percent)` - для выбора оптимального сервера
- `vpn_configs.public_key` - уникальный индекс
- `vpn_configs.ip_address` - уникальный индекс
- `traffic_stats(user_id, date)` - композитный индекс

## Начальные данные (Seeding)

При первом запуске автоматически создаются базовые тарифные планы.
Система проверяет наличие планов в базе и создает их только если они отсутствуют.

- **Basic** - 299₽/мес (1 устройство, 50 Мбит/с)
- **Premium** - 599₽/мес (5 устройств, безлимит)
- **Premium Year** - 5990₽/год (экономия 16%)

## Настройка подключения

Параметры подключения к базе данных задаются через переменные окружения в файле `configs/.env`.
Все параметры обязательны для корректной работы приложения.

```env
DB_HOST=localhost
DB_PORT=5432
DB_USER=spiritdb
DB_PASSWORD=your_password
DB_NAME=spiritdb
```

Пул подключений настроен:
- Max Idle Connections: 10
- Max Open Connections: 100
- Connection Max Lifetime: 1 час

## Вспомогательные методы моделей

Каждая модель предоставляет набор helper методов для упрощения работы с данными.
Эти методы инкапсулируют бизнес-логику и повышают читаемость кода.

### User
```go
user.Subscriptions // Все подписки пользователя
user.VPNConfigs    // Все конфигурации
user.Payments      // Все платежи
```

### Subscription
```go
subscription.IsExpired()  // Проверка истечения
subscription.DaysLeft()   // Дней до окончания
```

### VPNServer
```go
server.HasCapacity()  // Есть ли места
server.UpdateLoad()   // Обновить процент загрузки
```

### Payment
```go
payment.IsSuccessful()  // Успешен ли платеж
payment.IsPending()     // Ожидает обработки
```

### TrafficStat
```go
stat.TotalBytes()  // Общий трафик в байтах
stat.TotalGB()     // Общий трафик в ГБ
```
