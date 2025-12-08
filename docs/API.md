# API Документация SpiritVPN

## Базовая информация

- **Base URL:** `https://api.spiritvpn.com/api/v1`
- **Формат данных:** JSON
- **Аутентификация:** JWT Bearer Token
- **Rate Limit:** 100 запросов/минуту

## Аутентификация

Все защищенные эндпоинты требуют JWT токен в заголовке:

```
Authorization: Bearer <jwt_token>
```

---

## Аутентификация

### Регистрация

```http
POST /auth/register
```

**Request Body:**
```json
{
  "telegram_id": 123456789,
  "username": "user123",
  "email": "user@example.com"
}
```

**Response:** `201 Created`
```json
{
  "success": true,
  "data": {
    "user_id": 1,
    "telegram_id": 123456789,
    "username": "user123",
    "created_at": "2025-11-15T10:00:00Z"
  },
  "token": {
    "access_token": "eyJhbGciOiJIUzI1NiIs...",
    "refresh_token": "eyJhbGciOiJIUzI1NiIs...",
    "expires_in": 3600
  }
}
```

**Errors:**
- `400` - Неверные данные
- `409` - Пользователь уже существует

---

### Вход

```http
POST /auth/login
```

**Request Body:**
```json
{
  "telegram_id": 123456789
}
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "user_id": 1,
    "telegram_id": 123456789
  },
  "token": {
    "access_token": "eyJhbGciOiJIUzI1NiIs...",
    "refresh_token": "eyJhbGciOiJIUzI1NiIs...",
    "expires_in": 3600
  }
}
```

---

### Обновление токена

```http
POST /auth/refresh
```

**Request Body:**
```json
{
  "refresh_token": "eyJhbGciOiJIUzI1NiIs..."
}
```

**Response:** `200 OK`
```json
{
  "success": true,
  "token": {
    "access_token": "eyJhbGciOiJIUzI1NiIs...",
    "expires_in": 3600
  }
}
```

---

## Пользователи

### Получить профиль

```http
GET /users/:id
```

**Headers:**
```
Authorization: Bearer <token>
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "id": 1,
    "telegram_id": 123456789,
    "username": "user123",
    "email": "user@example.com",
    "subscription": {
      "plan": "premium",
      "active": true,
      "expires_at": "2025-12-15T10:00:00Z"
    },
    "created_at": "2025-11-15T10:00:00Z"
  }
}
```

---

### Обновить профиль

```http
PUT /users/:id
```

**Request Body:**
```json
{
  "username": "newusername",
  "email": "newemail@example.com"
}
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "id": 1,
    "username": "newusername",
    "email": "newemail@example.com",
    "updated_at": "2025-11-15T11:00:00Z"
  }
}
```

---

## Подписки

### Получить тарифные планы

```http
GET /subscriptions/plans
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": [
    {
      "id": 1,
      "name": "Basic",
      "duration_days": 30,
      "price": 299.00,
      "currency": "RUB",
      "features": [
        "1 устройство",
        "50 Мбит/с",
        "Базовая поддержка"
      ]
    },
    {
      "id": 2,
      "name": "Premium",
      "duration_days": 30,
      "price": 599.00,
      "currency": "RUB",
      "features": [
        "5 устройств",
        "Безлимитная скорость",
        "Приоритетная поддержка",
        "Доступ ко всем серверам"
      ]
    },
    {
      "id": 3,
      "name": "Premium Year",
      "duration_days": 365,
      "price": 5990.00,
      "currency": "RUB",
      "discount": "16%",
      "features": [
        "5 устройств",
        "Безлимитная скорость",
        "Приоритетная поддержка",
        "Доступ ко всем серверам"
      ]
    }
  ]
}
```

---

### Создать подписку

```http
POST /subscriptions
```

**Request Body:**
```json
{
  "user_id": 1,
  "plan_id": 2,
  "payment_method": "yookassa"
}
```

**Response:** `201 Created`
```json
{
  "success": true,
  "data": {
    "subscription_id": 123,
    "user_id": 1,
    "plan": "Premium",
    "start_date": "2025-11-15T10:00:00Z",
    "end_date": "2025-12-15T10:00:00Z",
    "is_active": false,
    "payment_url": "https://yookassa.ru/checkout/..."
  }
}
```

---

### Получить активную подписку

```http
GET /subscriptions/user/:user_id
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "id": 123,
    "plan": "Premium",
    "is_active": true,
    "start_date": "2025-11-15T10:00:00Z",
    "end_date": "2025-12-15T10:00:00Z",
    "auto_renew": true,
    "days_left": 30
  }
}
```

---

### Отменить подписку

```http
DELETE /subscriptions/:id
```

**Response:** `200 OK`
```json
{
  "success": true,
  "message": "Подписка отменена. Доступ сохраняется до 2025-12-15"
}
```

---

## VPN Конфигурации

### Получить конфиг

```http
GET /configs/user/:user_id
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "config_id": 456,
    "user_id": 1,
    "server": {
      "name": "Germany-1",
      "location": "Frankfurt",
      "ip": "45.123.45.67",
      "port": 443
    },
    "uuid": "550e8400-e29b-41d4-a716-446655440000",
    "flow": "xtls-rprx-vision",
    "vless_link": "vless://550e8400-e29b-41d4-a716-446655440000@45.123.45.67:443?security=reality&sni=google.com&fp=chrome&pbk=...&sid=...&type=tcp&flow=xtls-rprx-vision#SpiritVPN-Germany",
    "qr_code": "data:image/png;base64,iVBORw0KGgo...",
    "created_at": "2025-11-15T10:00:00Z"
  }
}
```

---

### Сгенерировать новый конфиг

```http
POST /configs
```

**Request Body:**
```json
{
  "user_id": 1,
  "server_id": 2
}
```

**Response:** `201 Created`
```json
{
  "success": true,
  "data": {
    "config_id": 457,
    "config_file": "[Interface]\n...",
    "qr_code": "data:image/png;base64,..."
  }
}
```

---

### Удалить конфиг

```http
DELETE /configs/:id
```

**Response:** `200 OK`
```json
{
  "success": true,
  "message": "Конфигурация удалена"
}
```

---

## Платежи

### Создать платеж

```http
POST /payments
```

**Request Body:**
```json
{
  "user_id": 1,
  "subscription_id": 123,
  "amount": 599.00,
  "currency": "RUB",
  "payment_method": "yookassa"
}
```

**Response:** `201 Created`
```json
{
  "success": true,
  "data": {
    "payment_id": 789,
    "amount": 599.00,
    "currency": "RUB",
    "status": "pending",
    "payment_url": "https://yookassa.ru/checkout/...",
    "expires_at": "2025-11-15T11:00:00Z"
  }
}
```

---

### Проверить статус платежа

```http
GET /payments/:id
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "payment_id": 789,
    "status": "succeeded",
    "amount": 599.00,
    "paid_at": "2025-11-15T10:30:00Z",
    "subscription_activated": true
  }
}
```

**Статусы:**
- `pending` - Ожидает оплаты
- `processing` - Обрабатывается
- `succeeded` - Успешно
- `failed` - Ошибка
- `cancelled` - Отменен

---

### Webhook для платежей

```http
POST /payments/webhook
```

**Request Body (YooKassa):**
```json
{
  "type": "notification",
  "event": "payment.succeeded",
  "object": {
    "id": "payment_id",
    "status": "succeeded",
    "amount": {
      "value": "599.00",
      "currency": "RUB"
    },
    "metadata": {
      "user_id": "1",
      "subscription_id": "123"
    }
  }
}
```

---

## Статистика

### Получить статистику пользователя

```http
GET /stats/user/:user_id
```

**Query Parameters:**
- `period` - day, week, month, year (default: month)

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "user_id": 1,
    "period": "month",
    "traffic": {
      "sent": 15728640000,
      "received": 52428800000,
      "total": 68157440000,
      "formatted": {
        "sent": "14.65 GB",
        "received": "48.83 GB",
        "total": "63.48 GB"
      }
    },
    "connection_time": 864000,
    "connection_time_formatted": "10 дней",
    "sessions": 45,
    "avg_speed": {
      "download": 125829120,
      "upload": 31457280,
      "formatted": {
        "download": "120 Мбит/с",
        "upload": "30 Мбит/с"
      }
    }
  }
}
```

---

### Получить общую статистику системы (Admin)

```http
GET /stats/system
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": {
    "total_users": 1523,
    "active_subscriptions": 892,
    "total_revenue": 532340.00,
    "servers": [
      {
        "name": "Germany-1",
        "active_users": 234,
        "load": 45.2,
        "status": "online"
      }
    ],
    "traffic_total": "15.2 TB"
  }
}
```

---

## 🖥️ Серверы

### Получить список серверов

```http
GET /servers
```

**Response:** `200 OK`
```json
{
  "success": true,
  "data": [
    {
      "id": 1,
      "name": "Germany-1",
      "location": "Frankfurt",
      "country_code": "DE",
      "ip": "45.123.45.67",
      "port": 51820,
      "load": 45.2,
      "ping": 15,
      "is_active": true,
      "current_users": 234,
      "max_users": 1000
    },
    {
      "id": 2,
      "name": "USA-1",
      "location": "New York",
      "country_code": "US",
      "ip": "104.123.45.67",
      "port": 51820,
      "load": 32.8,
      "ping": 85,
      "is_active": true,
      "current_users": 167,
      "max_users": 1000
    }
  ]
}
```

---

## Коды ошибок

### Формат ответа с ошибкой

```json
{
  "success": false,
  "error": {
    "code": "INVALID_TOKEN",
    "message": "JWT токен истек",
    "details": {}
  }
}
```

### HTTP статусы

- `200` - OK
- `201` - Created
- `400` - Bad Request
- `401` - Unauthorized
- `403` - Forbidden
- `404` - Not Found
- `409` - Conflict
- `429` - Too Many Requests
- `500` - Internal Server Error

### Коды ошибок приложения

| Код | Описание |
|-----|----------|
| `INVALID_REQUEST` | Неверный запрос |
| `INVALID_TOKEN` | Неверный токен |
| `TOKEN_EXPIRED` | Токен истек |
| `USER_NOT_FOUND` | Пользователь не найден |
| `USER_EXISTS` | Пользователь уже существует |
| `SUBSCRIPTION_NOT_FOUND` | Подписка не найдена |
| `SUBSCRIPTION_EXPIRED` | Подписка истекла |
| `PAYMENT_FAILED` | Ошибка платежа |
| `SERVER_UNAVAILABLE` | Сервер недоступен |
| `RATE_LIMIT_EXCEEDED` | Превышен лимит запросов |

---

## Пример использования

### Go Client

```go
package main

import (
    "bytes"
    "encoding/json"
    "net/http"
)

type LoginRequest struct {
    TelegramID int64 `json:"telegram_id"`
}

func main() {
    client := &http.Client{}
    
    // Логин
    body, _ := json.Marshal(LoginRequest{TelegramID: 123456789})
    req, _ := http.NewRequest("POST", "https://api.spiritvpn.com/api/v1/auth/login", bytes.NewBuffer(body))
    req.Header.Set("Content-Type", "application/json")
    
    resp, _ := client.Do(req)
    defer resp.Body.Close()
    
    // Получить конфиг
    req2, _ := http.NewRequest("GET", "https://api.spiritvpn.com/api/v1/configs/user/1", nil)
    req2.Header.Set("Authorization", "Bearer <token>")
    
    resp2, _ := client.Do(req2)
    defer resp2.Body.Close()
}
```

### Python Client

```python
import requests

# Логин
response = requests.post(
    'https://api.spiritvpn.com/api/v1/auth/login',
    json={'telegram_id': 123456789}
)
token = response.json()['token']['access_token']

# Получить конфиг
headers = {'Authorization': f'Bearer {token}'}
config = requests.get(
    'https://api.spiritvpn.com/api/v1/configs/user/1',
    headers=headers
).json()
```

---

## Changelog

### v1.0.0 (2025-11-15)
- ✅ Базовая аутентификация
- ✅ Управление пользователями
- ✅ Подписки и платежи
- ✅ Генерация конфигов
- ✅ Статистика

### Планируется в v1.1.0
- [ ] Реферальная система
- [ ] Push уведомления
- [ ] Расширенная аналитика
- [ ] Промокоды
