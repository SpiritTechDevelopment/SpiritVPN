# VLESS Protocol Documentation

## Описание

VLESS - это современный протокол для VPN подключений, разработанный как улучшенная версия VMess.
## Основные характеристики

- **Легковесность**: минимальные накладные расходы на обработку пакетов
- **Безопасность**: использование TLS 1.3 для шифрования канала
- **Производительность**: высокая скорость передачи данных
- **Reality**: технология маскировки трафика под легитимный HTTPS
- **Совместимость**: поддержка всех популярных клиентов

## Архитектура VLESS

### Компоненты

VLESS использует следующие компоненты:
- **Reality** - технология маскировки трафика под легитимный HTTPS
- **XTLS** - расширенный TLS для снижения накладных расходов
- **Vision Flow** - улучшенный контроль потока данных
- **UUID** - уникальный идентификатор пользователя для аутентификации

### Схема работы

```
┌──────────┐                    ┌───────────┐                    ┌──────────┐
│  Client  │ ◄──── TLS 1.3 ────►│   VLESS   │ ◄─── Internet ────►│  Target  │
│  (App)   │       Reality      │   Server  │                    │   Site   │
└──────────┘                    └───────────┘                    └──────────┘
```

Трафик маскируется под обычное HTTPS соединение к популярному сайту (SNI), что делает его неотличимым от легитимного трафика.

## Настройка сервера

### Генерация ключей

```bash
# Генерация X25519 private key
openssl rand -base64 32

# Или используя xray
xray x25519
```

Результат:
```
Private key: YHKvw5TGPWZzA8T2ZFQJL...
Public key: SLwxKXe-5KhtKkjLhNf...
```

### Конфигурация

Добавьте следующие переменные в `configs/.env`:

```env
# VPN Server Configuration
VPN_SERVER_PORT=443
VPN_SERVER_NAME=google.com
VPN_SHORT_IDS=6ba85179e30d4fc2
VPN_PRIVATE_KEY=YHKvw5TGPWZzA8T2ZFQJL...
VPN_PUBLIC_KEY=SLwxKXe-5KhtKkjLhNf...

# Server Details
VPN_SERVER_HOST=vpn.spiritvpn.com
VPN_FLOW=xtls-rprx-vision
```

**Параметры:**
- `VPN_SERVER_PORT` - порт сервера (рекомендуется 443)
- `VPN_SERVER_NAME` - SNI для маскировки (например, google.com, cloudflare.com)
- `VPN_SHORT_IDS` - короткие идентификаторы для маскировки
- `VPN_PRIVATE_KEY` - приватный X25519 ключ
- `VPN_PUBLIC_KEY` - публичный X25519 ключ для клиентов
- `VPN_FLOW` - тип flow (`xtls-rprx-vision` или `xtls-rprx-direct`)

### Запуск сервера

```bash
# Через Docker
docker-compose up vpn-server

# Напрямую
go run cmd/vpn-server/main.go

# С логированием
go run cmd/vpn-server/main.go --log-level debug
```

## Клиентская конфигурация

### Формат VLESS URL

```
vless://UUID@SERVER:PORT?encryption=none&security=SECURITY&sni=SNI&fp=FINGERPRINT&pbk=PUBLIC_KEY&sid=SHORT_ID&type=TYPE&flow=FLOW#NAME
```

**Параметры:**
- `UUID` - уникальный идентификатор пользователя
- `SERVER` - адрес сервера
- `PORT` - порт сервера
- `encryption` - тип шифрования (обычно `none`, т.к. используется TLS)
- `security` - тип безопасности (`reality` или `tls`)
- `sni` - Server Name Indication для маскировки
- `fp` - fingerprint браузера (`chrome`, `firefox`, `safari`)
- `pbk` - public key сервера
- `sid` - short ID
- `type` - тип транспорта (`tcp`, `ws`, `grpc`)
- `flow` - тип flow (`xtls-rprx-vision`)
- `NAME` - имя конфигурации

### Пример конфигурации

```
vless://550e8400-e29b-41d4-a716-446655440000@vpn.spiritvpn.com:443?encryption=none&security=reality&sni=google.com&fp=chrome&pbk=SLwxKXe-5KhtKkjLhNf...&sid=6ba85179e30d4fc2&type=tcp&flow=xtls-rprx-vision#SpiritVPN
```

### Получение конфигурации

#### Через Telegram бота

```
/config
```

Бот вернет VLESS URL и QR-код.

#### Через API

```bash
curl -X POST https://api.spiritvpn.com/api/v1/vpn/config \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "server_id": 2}'
```

Подробнее см. [docs/API.md](API.md)

## Поддерживаемые клиенты

### Windows

**v2rayN** (рекомендуется)
- Скачать: [GitHub](https://github.com/2dust/v2rayN/releases)
- Установка: распакуйте архив и запустите `v2rayN.exe`
- Импорт конфигурации:
  - Сканирование QR-кода
  - Импорт из буфера обмена (скопируйте VLESS URL)

### Android

**v2rayNG**
- Скачать: [Google Play](https://play.google.com/store/apps/details?id=com.v2ray.ang) или [GitHub](https://github.com/2dust/v2rayNG/releases)
- Импорт: сканируйте QR-код или вставьте VLESS URL

### iOS

**Shadowrocket** (платное)
- Скачать: [App Store](https://apps.apple.com/app/shadowrocket/id932747118)
- Импорт: сканируйте QR-код или добавьте вручную

### Linux/macOS/Windows

**Nekoray** (кроссплатформенный)
- Скачать: [GitHub](https://github.com/MatsuriDayo/nekoray/releases)
- Поддержка всех платформ
- Удобный GUI

**Qv2ray** (устаревший, но стабильный)
- Скачать: [GitHub](https://github.com/Qv2ray/Qv2ray/releases)
- Кроссплатформенный клиент

## Технические детали

### Безопасность

**Аутентификация:**
- UUID для идентификации пользователя
- Проверка на стороне сервера
- Защита от replay атак

**Шифрование:**
- TLS 1.3 для канала связи
- Reality для маскировки
- X25519 для обмена ключами

**Защита от обнаружения:**
- Маскировка под легитимный HTTPS трафик
- SNI подмена для имитации доступа к популярным сайтам
- Fingerprint браузера для имитации реального браузера

### Производительность

**Оптимизации:**
- Минимальные накладные расходы протокола
- XTLS для уменьшения двойного шифрования
- Vision Flow для оптимального управления потоком

**Типичная производительность:**
- Latency: +5-15ms по сравнению с прямым подключением
- Throughput: 90-99% от скорости канала
- CPU usage: низкое потребление ресурсов

### Flow типы

**xtls-rprx-vision** (рекомендуется)
- Лучшая защита от обнаружения
- Оптимальная производительность
- Подходит для большинства сценариев

**xtls-rprx-direct**
- Максимальная производительность
- Меньше защита от обнаружения
- Подходит для стабильных каналов

## Мониторинг и диагностика

### Проверка статуса подключения

Через API:
```bash
curl -H "Authorization: Bearer YOUR_TOKEN" \
  https://api.spiritvpn.com/api/v1/vpn/status
```

Через Telegram бота:
```
/status
```

### Логи сервера

```bash
# Docker
docker-compose logs vpn-server

# Systemd
journalctl -u vpn-server -f

# Файлы
tail -f logs/vpn-server.log
```

### Метрики

Доступные метрики:
- Количество активных подключений
- Трафик (входящий/исходящий)
- Ошибки подключения
- Время работы сервера

## Troubleshooting

### Проблемы с подключением

**Не удается подключиться**

1. Проверьте правильность UUID
2. Убедитесь, что порт не заблокирован файрволом
3. Проверьте корректность SNI и Server Name
4. Попробуйте другой сервер

**Проверка подключения:**
```bash
# Проверка доступности порта
telnet vpn.spiritvpn.com 443

# Проверка DNS
nslookup vpn.spiritvpn.com
```

### Низкая скорость

**Возможные причины:**
1. Перегрузка сервера
2. Проблемы с каналом
3. Неоптимальный Flow

**Решения:**
1. Попробуйте другой Flow (`xtls-rprx-direct`)
2. Измените транспорт с TCP на WebSocket
3. Выберите другой сервер ближе к вам
4. Проверьте нагрузку на сервер через API

### Периодические разрывы соединения

**Решения:**
1. Увеличьте keepalive интервал
2. Измените SNI на другой домен
3. Используйте другой fingerprint браузера
4. Проверьте стабильность интернет-канала

### Обнаружение и блокировка

**Признаки:**
- Постоянные разрывы соединения
- Медленная скорость
- Невозможность подключения в определенное время

**Решения:**
1. Смените SNI на более популярный домен
2. Измените порт (попробуйте 8443, 2053)
3. Используйте WebSocket транспорт
4. Смените публичный ключ сервера

## Лучшие практики

### Для пользователей

1. **Используйте актуальные клиенты** - регулярно обновляйте приложения
2. **Не делитесь конфигурацией** - каждый UUID уникален
3. **Выбирайте ближайший сервер** - для лучшей скорости
4. **Проверяйте утечки** - используйте сервисы проверки IP

### Для администраторов

1. **Регулярно обновляйте Xray** - для получения последних улучшений
2. **Мониторьте нагрузку** - не допускайте перегрузки серверов
3. **Используйте популярные SNI** - google.com, cloudflare.com и т.д.
4. **Ротируйте ключи** - периодически меняйте X25519 ключи
5. **Настройте логирование** - для диагностики проблем

## Дополнительная информация

### Документация

- [Архитектура SpiritVPN](ARCHITECTURE.md)
- [API документация](API.md)
- [Развертывание](DEPLOYMENT.md)
- [FAQ](FAQ.md)

### Внешние ресурсы

- [Xray-core документация](https://xtls.github.io/)
- [VLESS спецификация](https://github.com/XTLS/Xray-core)
- [Reality протокол](https://github.com/XTLS/REALITY)

### Поддержка

При возникновении проблем:
1. Проверьте FAQ
2. Проверьте логи сервера
3. Обратитесь в поддержку через Telegram
4. Создайте issue в GitHub

---

**Документация обновлена:** 06.01.2026
**Версия:** 1.0
