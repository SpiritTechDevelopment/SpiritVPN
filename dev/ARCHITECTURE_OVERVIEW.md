# SpiritVPN backend: обзор архитектуры

Ориентир для входа в проект. Нормативная истина — [`BACKEND_DOMAIN_AGREEMENTS.md`](BACKEND_DOMAIN_AGREEMENTS.md);
здесь — картина целиком, без деталей контракта. При расхождении верен нормативный документ.

## Три плоскости: кто чем владеет

```text
┌─ INFRA (Ansible / CI-CD) ─────────────────────────────────────────┐
│  топология (manifest), Xray-конфиг + outbounds + egress_tag,       │
│  node-agent, WireGuard, сертификаты. Владеет "железом и трубами".  │
└───────────────┬───────────────────────────────────────────────────┘
                │ manifest (gRPC)
┌─ BACKEND (control plane) ──────────────────────────────────────────┐
│  кто получает доступ, куда, на какой срок и квоту.                  │
│  PostgreSQL = единственный источник desired-state.                 │
└───┬───────────────────────────────────┬───────────────────────────┘
    │ Apply / GetLinks (gRPC)            │ push  EnsureUser* / ReconcileUsers
    │                                    │ pull  GetNodeState
┌─ PRODUCT (биллинг / бот) ─┐     ┌─ NODE-AGENT (на каждой ноде) ──────┐
│  продаёт подписки          │     │  единственный, кто трогает Xray    │
└────────────────────────────┘     │  (Handler / Stats / Routing API)   │
                                   └─────────────────────────────────────┘
```

Разделение жёсткое: **infra владеет топологией, backend — доступом клиентов,
agent — рантаймом Xray.** Backend к Xray напрямую не обращается.

## Компоненты backend

- **API** (gRPC): customer-команды (`ApplyCustomerAccess`, `GetCustomerAccessLinks`)
  и приём manifest от infra CI-CD.
- **Workers**: материализуют manifest, доставляют agent-операции (push), тянут
  usage (pull), гасят по expiry. Координация — только через PostgreSQL locks/leases.
- **PostgreSQL**: единственный central authority desired-state; готовый VLESS-URI
  не хранится, `client_uuid` зашифрован.
- Роли `api`/`worker` — одна кодовая база; в dev один процесс, в prod раздельно.

## Как это работает end-to-end

1. **Топология.** Infra рендерит manifest (ноды, fleets, bridge-relations с
   `egress_tag`) → gRPC в backend → проекция в PostgreSQL. Это карта, куда можно
   продавать доступ.
2. **Клиент покупает.** Product зовёт `ApplyCustomerAccess(customer_id, fleet,
   quota, expiry, command_number)`. Backend раскладывает в набор access: один
   FREEDOM на каждую ноду + один BRIDGE на каждую relation. У каждого — свой
   `client_uuid`, `accounting_id`, `egress_key`. Durable в PostgreSQL + очередь
   операций.
3. **Провижининг.** Worker пушит `EnsureUserPresent(User)`. Агент делает `AddUser`
   (уникальный email) **+** `AddRule` (`user:[email] → нужный outbound`) — на лету,
   без reload Xray.
4. **Подключение.** `GetCustomerAccessLinks` → backend строит VLESS-ссылки из
   manifest + расшифрованного uuid → юзер импортирует и подключается.
5. **Учёт и лимиты (pull).** Агент читает Xray-статистику per-email в локальный
   durable spool. Backend тянет `GetNodeState` + подтверждает курсор, начисляет
   per-node квоту; при превышении: `exhausted_at` → `EnsureUserAbsent` +
   `RemoveRule`. Expiry гасит все ноды разом.
6. **Восстановление (двухслойное).** Рестарт Xray стирает users и rules → агент
   немедленно поднимает их **add-only** из локального durable снапшота (быстро, даже
   при недоступном backend) → backend позже сходится **авторитетно** через
   `ReconcileUsers` (единственный источник удалений и коррекции дрейфа).

## Ключевые свойства

- **Desired-state реконсиляция:** PostgreSQL — истина, ноды сходятся к ней; потеря
  ответа/рестарт безопасны (идемпотентность).
- **Порядок команд** защищён `command_number` (реордер/повтор не портят состояние).
- **Fan-out** на runtime `RoutingService.AddRule` (≤2000 правил/ноду — дёшево).
- **Учёт per-customer** только на терминирующей ноде (вход для BRIDGE, exit для
  FREEDOM); приблизителен осознанно (потеря дельты уменьшает total; renewal
  обнуляет период по event-time).
- **Транспорт** backend↔agent — mTLS поверх WireGuard.

## Что это даёт юзерам

- **Мультилокационный fleet** — несколько точек входа/выхода в одной подписке.
- **FREEDOM** — прямой выход через нужную страну.
- **BRIDGE** — многохоп для устойчивости к блокировкам: «чистый» вход (выглядит как
  обычный TLS-сайт, REALITY) → выход в другой стране.
- **Fan-out** — с одного входа выбор из нескольких выходов.
- **Несколько рабочих ссылок сразу** + частичная готовность (рабочие ссылки
  выдаются, даже если часть нод ещё поднимается).
- **Time + data** — доступ по сроку и по трафику; **продление** = свежий период
  квоты без переноса старого расхода.

## Пока НЕ доступно (deferred)

- **Лимит по IP/устройствам** (анти-шаринг) — плумбинг (`SourceActivity`) готов,
  enforcement отложен.
- **Мгновенный ручной revoke** — в v1 нет; доступ гаснет по expiry/квоте.
- **Скорость на юзера** — не нативна для Xray; как фича не заложена.

## Перед стартом реализации (внешние зависимости)

1. Завендорить `contracts/nodeagent/v1/node_agent.proto` в backend-репо как
   замороженный baseline + change-request на поле `User.egress_key` с владельцем
   agent-стороны.
2. Согласованный manifest-proto с infra CI-CD (customer-сервис почти финал).
3. Перф-спайк routing-матчинга при ≤2000/ноду (низкий риск, в нагрузочный тест).

> Заметка: спека описывает **новый** backend и заменяет текущий `internal/vpn/*`
> (GORM + прямой Xray-клиент), а не расширяет его — объём оценивать как greenfield.
