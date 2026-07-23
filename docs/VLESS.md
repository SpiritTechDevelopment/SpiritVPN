# VLESS и Xray в SpiritVPN

## Назначение

SpiritVPN использует Xray-core как VPN-ядро и ориентирован на работу по схеме **VLESS + Reality**. Данный документ описывает базовые параметры конфигурации, формат клиентского подключения и точки контроля при эксплуатации.

## Основные характеристики

* протокол VLESS для клиентских подключений;
* Reality для маскировки трафика;
* TLS 1.3 как базовый уровень защиты канала;
* X25519-ключи для механизма Reality;
* интеграция с Xray API для управления пользователями и статистикой.

## Компоненты схемы

В текущей архитектуре участвуют:

* **Xray-core** — VPN-ядро;
* **VPN Server** — сервис проекта, взаимодействующий с Xray;
* **API Server** — служебный доступ к операциям управления;
* **Database Layer** — хранение пользователей, подписок, серверов и VPN-конфигураций.

## Конфигурация приложения

Основные параметры VPN-сервера задаются в `configs/.env`:

```env
VPN_HOST=localhost
VPN_PORT=443
VPN_API_PORT=10085
VPN_API_ADDRESS=localhost
VPN_INBOUND_TAG=vless-in
VPN_NODE_NAME=entry-1
VPN_ENDPOINTS_FILE=
VPN_SERVER_NAME=google.com
VPN_PRIVATE_KEY=
VPN_PUBLIC_KEY=
VPN_SHORT_IDS=
VPN_FINGERPRINT=chrome
VPN_TEST_ACCESS_TTL=24h
VPN_STATS_INTERVAL=5m
INTERNAL_API_TOKEN=
```

### Описание параметров

* `VPN_HOST` — адрес VPN-сервера;
* `VPN_PORT` — внешний порт для клиентских подключений;
* `VPN_API_PORT` — порт Xray API;
* `VPN_API_ADDRESS` — адрес Xray API;
* `VPN_INBOUND_TAG` — клиентский inbound tag Xray;
* `VPN_NODE_NAME` — стабильное имя entry-ноды;
* `VPN_ENDPOINTS_FILE` — путь к защищённому `client-endpoints.json` из Infrastructure;
* `VPN_SERVER_NAME` — SNI/Server Name для Reality;
* `VPN_PRIVATE_KEY` — приватный X25519-ключ сервера;
* `VPN_PUBLIC_KEY` — публичный X25519-ключ сервера;
* `VPN_SHORT_IDS` — short IDs для Reality;
* `VPN_FINGERPRINT` — TLS fingerprint в клиентской ссылке;
* `VPN_TEST_ACCESS_TTL` — срок тестового доступа, выдаваемого через internal API;
* `INTERNAL_API_TOKEN` — Bearer token для service-to-service вызовов backend API;
* `VPN_STATS_INTERVAL` — интервал сбора статистики трафика.

## Конфигурация Xray

Базовый конфигурационный файл Xray расположен по пути:

```text
configs/xray.json
```

При изменении параметров приложения рекомендуется поддерживать согласованность между `configs/.env` и `configs/xray.json`.

## Генерация ключей Reality

Для Reality используются X25519-ключи. Ключи можно сгенерировать с помощью Xray:

```bash
xray x25519
```

Результат содержит:

* `Private key`
* `Public key`

Полученные значения необходимо перенести в `VPN_PRIVATE_KEY` и `VPN_PUBLIC_KEY`.

## Запуск VPN-сервера

### Локальный запуск

```bash
go run cmd/vpn-server/main.go
```

### Запуск через Docker Compose

```bash
docker-compose up -d vpn-server
```

## Интеграция с Xray API

Внутренний слой `internal/vpn` использует Xray API для следующих операций:

* подключение к API Xray;
* добавление пользователя;
* удаление пользователя;
* получение статистики трафика.

Для подключения используются `VPN_API_ADDRESS` и `VPN_API_PORT`.

## Клиентская конфигурация VLESS

Общий формат клиентской ссылки VLESS:

```text
vless://UUID@SERVER:PORT?encryption=none&security=reality&sni=SNI&fp=chrome&pbk=PUBLIC_KEY&sid=SHORT_ID&type=tcp&flow=xtls-rprx-vision#NAME
```

### Основные параметры

* `UUID` — идентификатор пользователя;
* `SERVER` — адрес сервера;
* `PORT` — порт подключения;
* `security=reality` — режим безопасности;
* `sni` — server name для маскировки;
* `fp` — fingerprint клиента;
* `pbk` — публичный ключ Reality;
* `sid` — short ID;
* `type=tcp` — транспорт;
* `flow=xtls-rprx-vision` — рекомендуемый flow.

## Модель данных VPN-конфигурации

В проекте для клиентских VPN-конфигураций используется модель `VPNConfig`, содержащая:

* пользователя;
* подписку;
* VPN-сервер;
* UUID;
* flow;
* временные метки создания и обновления.

## Статистика трафика

Для сбора статистики предусмотрены:

* модель `TrafficStat`;
* worker в `internal/workers`;
* взаимодействие с Xray API через внутренний VPN-слой.

Интервал сбора задается переменной:

```env
VPN_STATS_INTERVAL=5m
```

## Smoke-проверка Xray API

В каталоге `test/smoke/` находится smoke-сценарий проверки основных операций с Xray API:

* подключение к API;
* добавление тестового пользователя;
* получение статистики;
* удаление пользователя.

## Поддерживаемые клиенты

Для VLESS + Reality можно использовать клиенты с поддержкой Xray/V2Ray-экосистемы, включая:

* v2rayN
* v2rayNG
* Nekoray
* Shadowrocket

Конкретный клиент выбирается в зависимости от целевой платформы и требований развертывания.

## Диагностика

### Проверка конфигурации

Проверьте:

* корректность `VPN_HOST` и `VPN_PORT`;
* корректность `VPN_API_ADDRESS` и `VPN_API_PORT`;
* наличие и валидность `VPN_PRIVATE_KEY` и `VPN_PUBLIC_KEY`;
* согласованность `VPN_SERVER_NAME` и параметров Reality в Xray.

### Проверка логов

Для диагностики используйте:

```bash
docker-compose logs vpn-server
```

или системные журналы/файлы логов приложения, если настроено файловое логирование.

### Типовые причины проблем с подключением

* некорректный UUID;
* несоответствие SNI и Reality-настроек;
* недоступность внешнего порта;
* недоступность Xray API;
* несогласованные ключи Reality.

## Рекомендации по эксплуатации

* использовать отдельные UUID для каждого пользователя;
* хранить приватные ключи вне репозитория;
* регулярно обновлять Xray-core;
* контролировать нагрузку по данным статистики;
* включать структурированное логирование для серверных компонентов.
