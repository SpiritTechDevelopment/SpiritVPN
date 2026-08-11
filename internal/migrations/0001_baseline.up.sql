-- Базовая схема backend SpiritVPN.
-- Нормативный источник: BACKEND_DOMAIN_AGREEMENTS.md §11 (PostgreSQL model) и §11.1.
--
-- Соглашения:
--   * Все timestamps — UTC timestamptz.
--   * uint64-значения с провода (байтовые counters, command_number, sequence спула,
--     байты квоты) хранятся как numeric(20,0); int64/внутренние счётчики backend —
--     как bigint.
--   * Enum-подобные колонки — text + CHECK; нативный ENUM PostgreSQL не используем,
--     чтобы набор значений эволюционировал обычными миграциями.
--   * Никакого ON DELETE CASCADE: v1 не удаляет историю (§4, §11); FK нужны только
--     для ссылочной целостности строк, которые помечаются, а не удаляются.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Группа A. Проекция топологии (authority: infrastructure manifest, §6)
-- ---------------------------------------------------------------------------

-- Журнал принятых manifest-снапшотов. revision глобальна и строго возрастает
-- (проверяется в коде приложения); digest отличает идемпотентный повтор от
-- конфликтующего (§6).
CREATE TABLE manifest_revisions (
    revision          bigint      PRIMARY KEY,
    digest            text        NOT NULL,
    canonical_payload jsonb       NOT NULL,
    applied_at        timestamptz NOT NULL DEFAULT now()
);

-- Логические VPN-fleets. Принятый vpn_fleet_id не удаляется и не переиспользуется;
-- fleet может быть пустым (§4). current=false — отсутствует в последнем снапшоте.
CREATE TABLE vpn_fleets (
    vpn_fleet_id     bigint      PRIMARY KEY,
    manifest_revision bigint     NOT NULL REFERENCES manifest_revisions (revision),
    current          boolean     NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- Логические ноды. node_id — стабильная идентичность (одна строка навсегда);
-- agent/public config изменяемы между revisions. desired_revision — внутренний
-- монотонный счётчик желаемого набора юзеров ноды, увеличивается один раз на
-- mutating-транзакцию и гейтит приём ReconcileUsers (§10).
CREATE TABLE vpn_nodes (
    node_id           text        PRIMARY KEY,
    agent_config      jsonb       NOT NULL,
    public_config     jsonb       NOT NULL,
    desired_revision  bigint      NOT NULL DEFAULT 1 CHECK (desired_revision > 0),
    manifest_revision bigint      NOT NULL REFERENCES manifest_revisions (revision),
    current           boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- Membership fleet<->нода. current=false — нода покинула fleet.
CREATE TABLE vpn_fleet_nodes (
    vpn_fleet_id      bigint      NOT NULL REFERENCES vpn_fleets (vpn_fleet_id),
    node_id           text        NOT NULL REFERENCES vpn_nodes (node_id),
    manifest_revision bigint      NOT NULL REFERENCES manifest_revisions (revision),
    current           boolean     NOT NULL DEFAULT true,
    PRIMARY KEY (vpn_fleet_id, node_id)
);

-- Направленные BRIDGE-связи (entry -> exit). routing_key — стабильная идентичность
-- внутри fleet; пара (entry, exit) уникальна в fleet и неизменяема для
-- существующего routing_key (§6). egress_tag передаётся агенту дословно как
-- User.egress_key (§7).
CREATE TABLE vpn_bridge_routes (
    vpn_fleet_id      bigint      NOT NULL REFERENCES vpn_fleets (vpn_fleet_id),
    routing_key       text        NOT NULL,
    entry_node_id     text        NOT NULL REFERENCES vpn_nodes (node_id),
    exit_node_id      text        NOT NULL REFERENCES vpn_nodes (node_id),
    egress_tag        text        NOT NULL CHECK (egress_tag <> ''),
    display_name      text        NOT NULL,
    manifest_revision bigint      NOT NULL REFERENCES manifest_revisions (revision),
    current           boolean     NOT NULL DEFAULT true,
    PRIMARY KEY (vpn_fleet_id, routing_key),
    CONSTRAINT vpn_bridge_routes_entry_exit_distinct CHECK (entry_node_id <> exit_node_id)
);

-- ---------------------------------------------------------------------------
-- Группа B. Желаемое состояние customer (authority: backend, §4, §5)
-- ---------------------------------------------------------------------------

-- Корневая строка, ровно одна на customer. customer_id непрозрачен (1..256 байт).
-- Customer привязан максимум к одному fleet (v1). last_command_number — гейт
-- порядка: применяется только строго больший command_number (§5). Эта строка —
-- точка сериализации SELECT ... FOR UPDATE для всех изменений одного customer (§11.1).
CREATE TABLE customer_entitlements (
    customer_id         text          PRIMARY KEY,
    vpn_fleet_id        bigint        NOT NULL REFERENCES vpn_fleets (vpn_fleet_id),
    expires_at          timestamptz   NOT NULL,
    desired_version     bigint        NOT NULL DEFAULT 0,
    last_command_number numeric(20,0) NOT NULL DEFAULT 0 CHECK (last_command_number >= 0),
    created_at          timestamptz   NOT NULL DEFAULT now(),
    updated_at          timestamptz   NOT NULL DEFAULT now()
);

-- Периоды квоты. Renewal закрывает текущий период и открывает новый тем же
-- timestamp, поэтому периоды — непересекающиеся полуоткрытые интервалы
-- [started_at, closed_at). Единственный открытый период (closed_at IS NULL) —
-- текущий (§11).
CREATE TABLE quota_periods (
    quota_period_id   uuid          PRIMARY KEY,
    customer_id       text          NOT NULL REFERENCES customer_entitlements (customer_id),
    started_at        timestamptz   NOT NULL,
    closed_at         timestamptz,
    usage_quota_bytes numeric(20,0) NOT NULL CHECK (usage_quota_bytes > 0),
    CONSTRAINT quota_periods_interval CHECK (closed_at IS NULL OR started_at <= closed_at)
);

-- Трафик по каждой ноде внутри периода. total_bytes — stored generated column.
-- exhausted_at — одновременно факт исчерпания квоты и причина node-level блокировки;
-- отдельной таблицы блокировок нет (§11).
CREATE TABLE node_quota_usage (
    quota_period_id uuid          NOT NULL REFERENCES quota_periods (quota_period_id),
    node_id         text          NOT NULL REFERENCES vpn_nodes (node_id),
    uplink_bytes    numeric(20,0) NOT NULL DEFAULT 0 CHECK (uplink_bytes >= 0),
    downlink_bytes  numeric(20,0) NOT NULL DEFAULT 0 CHECK (downlink_bytes >= 0),
    total_bytes     numeric(20,0) GENERATED ALWAYS AS (uplink_bytes + downlink_bytes) STORED,
    exhausted_at    timestamptz,
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (quota_period_id, node_id)
);

-- Индивидуальный customer access = один FREEDOM на ноду либо один BRIDGE на
-- relation. У каждого свой зашифрованный client_uuid и глобально уникальный
-- accounting_id. retired_at помечает цель, удалённую из manifest; строка хранится
-- вечно, а повторное добавление даёт новое поколение с новыми credentials (§4).
CREATE TABLE vpn_accesses (
    access_id             uuid        PRIMARY KEY,
    customer_id           text        NOT NULL REFERENCES customer_entitlements (customer_id),
    kind                  text        NOT NULL CHECK (kind IN ('FREEDOM', 'BRIDGE')),
    logical_target_key    text        NOT NULL,
    generation            integer     NOT NULL CHECK (generation >= 1),
    entry_node_id         text        NOT NULL REFERENCES vpn_nodes (node_id),
    egress_key            text        NOT NULL DEFAULT '',
    accounting_id         text        NOT NULL UNIQUE,
    encrypted_client_uuid bytea       NOT NULL,
    encryption_key_id     text        NOT NULL,
    desired_state         text        NOT NULL CHECK (desired_state IN ('PRESENT', 'ABSENT')),
    apply_state           text        NOT NULL CHECK (apply_state IN ('PENDING', 'APPLIED', 'RETRYING', 'FAILED')),
    desired_version       bigint      NOT NULL DEFAULT 0,
    created_at            timestamptz NOT NULL DEFAULT now(),
    retired_at            timestamptz,
    CONSTRAINT vpn_accesses_generation_unique UNIQUE (customer_id, kind, logical_target_key, generation)
);

-- ---------------------------------------------------------------------------
-- Группа C. Доставка agent-операций (transactional outbox + журнал исполнения, §9)
-- ---------------------------------------------------------------------------

-- Одна строка на операцию; она же outbox. operation_id стабилен между retry; новая
-- desired_version создаёт новую операцию. Payload (User с client_uuid / egress_key)
-- здесь НЕ хранится — он пересобирается из строки access прямо перед RPC (§9).
-- lease_owner/lease_expires_at гейтят single-dispatch на ноду (§11.1).
CREATE TABLE agent_operations (
    operation_id       uuid        PRIMARY KEY,
    node_id            text        NOT NULL REFERENCES vpn_nodes (node_id),
    access_id          uuid        NOT NULL REFERENCES vpn_accesses (access_id),
    operation_type     text        NOT NULL CHECK (operation_type IN ('ENSURE_PRESENT', 'ENSURE_ABSENT')),
    desired_version    bigint      NOT NULL,
    status             text        NOT NULL CHECK (status IN (
                                       'PENDING', 'IN_FLIGHT', 'SUCCEEDED',
                                       'RETRY_WAIT', 'FAILED_PERMANENT', 'SUPERSEDED')),
    attempt_count      integer     NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at    timestamptz,
    lease_owner        text,
    lease_expires_at   timestamptz,
    last_error_code    text,
    last_error_message text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz,
    CONSTRAINT agent_operations_access_version_unique UNIQUE (access_id, desired_version)
);

-- Durable-джоба для fan-out customer access по всем customer после commit manifest,
-- обрабатывается worker'ами пакетами, а не внутри RPC-транзакции manifest (§6, §13).
CREATE TABLE manifest_materialization_jobs (
    revision         bigint      PRIMARY KEY REFERENCES manifest_revisions (revision),
    cursor           jsonb,
    status           text        NOT NULL CHECK (status IN ('PENDING', 'IN_PROGRESS', 'DONE', 'FAILED')),
    lease_owner      text,
    lease_expires_at timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Группа D. Учёт трафика через pull от агента (§12)
-- ---------------------------------------------------------------------------

-- Подтверждённая позиция usage-спула по ноде. Агент удаляет только подтверждённые
-- batches; новый spool_id сбрасывает sequence в ноль (§12).
--
-- Она же lease-таблица pull worker'а: §12 требует, чтобы на ноду одновременно был
-- активен один worker, а отдельная таблица ради двух колонок избыточна. Строка
-- заводится лениво при первом claim, до всякого успешного pull (решение 60).
--
-- updated_at означает «последняя ПОПЫТКА pull», а не «последний сдвиг курсора»:
-- иначе нода без трафика опрашивалась бы циклом без пауз (решение 61).
CREATE TABLE node_usage_cursors (
    node_id          text          PRIMARY KEY REFERENCES vpn_nodes (node_id),
    spool_id         text          NOT NULL,
    acked_sequence   numeric(20,0) NOT NULL DEFAULT 0 CHECK (acked_sequence >= 0),
    lease_owner      text,
    lease_expires_at timestamptz,
    updated_at       timestamptz   NOT NULL DEFAULT now()
);

-- Реестр идемпотентности: один usage-item начисляется ровно один раз, пока жива его
-- строка. Вставка через ON CONFLICT DO NOTHING RETURNING; попадание = уже начислено
-- (§12). access_id / quota_period_id nullable для карантинных items.
CREATE TABLE traffic_usage_items_processed (
    node_id         text          NOT NULL,
    spool_id        text          NOT NULL,
    sequence        numeric(20,0) NOT NULL,
    accounting_id   text          NOT NULL,
    access_id       uuid          REFERENCES vpn_accesses (access_id),
    quota_period_id uuid          REFERENCES quota_periods (quota_period_id),
    result          text          NOT NULL CHECK (result IN ('APPLIED', 'IGNORED_CLOSED_PERIOD', 'QUARANTINED')),
    processed_at    timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, spool_id, sequence, accounting_id)
);

-- Карантин для неизвестных accounting_id / некорректных items, чтобы один плохой
-- item не блокировал batch (§12). Только sanitized payload; никаких сырых секретов.
CREATE TABLE traffic_batch_quarantine (
    quarantine_id     uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id           text          NOT NULL,
    spool_id          text          NOT NULL,
    sequence          numeric(20,0) NOT NULL,
    accounting_id     text,
    reason            text          NOT NULL,
    sanitized_payload jsonb,
    created_at        timestamptz   NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Группа E. Аудит (append-only для runtime-роли, §15)
-- ---------------------------------------------------------------------------

CREATE TABLE audit_events (
    audit_id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    occurred_at        timestamptz NOT NULL DEFAULT now(),
    actor_type         text        NOT NULL,
    actor_id           text,
    action             text        NOT NULL,
    target_type        text,
    target_id          text,
    request_id         text,
    outcome            text        NOT NULL,
    sanitized_metadata jsonb
);

-- ---------------------------------------------------------------------------
-- Runtime-индексы сверх primary/unique ключей (§11)
-- ---------------------------------------------------------------------------

-- Максимум один открытый период квоты на customer.
CREATE UNIQUE INDEX quota_periods_one_open_per_customer
    ON quota_periods (customer_id)
    WHERE closed_at IS NULL;

-- Выбор периода по collected_at (event-time), сначала последние.
CREATE INDEX quota_periods_customer_started_at
    ON quota_periods (customer_id, started_at DESC);

-- Пара (entry_node_id, exit_node_id) уникальна среди ТЕКУЩИХ связей fleet (§6).
--
-- Частичный индекс, а не табличное ограничение UNIQUE: удалённая связь физически
-- остаётся строкой с current = false (§6, история не удаляется), и полное
-- ограничение навсегда закрепило бы за ней её пару. Перенос route, который §6
-- разрешает («удаление старого и добавление нового routing_key»), стал бы тогда
-- невыполним — новый routing_key с той же парой упёрся бы в мёртвую строку.
CREATE UNIQUE INDEX vpn_bridge_routes_current_pair
    ON vpn_bridge_routes (vpn_fleet_id, entry_node_id, exit_node_id)
    WHERE current;

-- Путь выборки due customer для expiry worker (§13).
--
-- Порядок колонок совпадает с ORDER BY запроса, поэтому сортировка не нужна и
-- сканирование останавливается на первом подходящем customer. Partial index с
-- условием на now() построить нельзя — оно не immutable.
CREATE INDEX customer_entitlements_expiry
    ON customer_entitlements (expires_at, customer_id);

-- Единственный текущий (не retired) access на logical target; он же путь поиска
-- для Customer API.
CREATE UNIQUE INDEX vpn_accesses_current_target
    ON vpn_accesses (customer_id, kind, logical_target_key)
    WHERE retired_at IS NULL;

-- Снапшот ноды для ReconcileUsers: present-юзеры на ноде.
CREATE INDEX vpn_accesses_node_snapshot
    ON vpn_accesses (entry_node_id, access_id)
    WHERE retired_at IS NULL AND desired_state = 'PRESENT';

-- Retryable-операции, готовые к dispatch, по времени и ноде.
CREATE INDEX agent_operations_retryable
    ON agent_operations (next_attempt_at, node_id, operation_id)
    WHERE status IN ('PENDING', 'RETRY_WAIT');

-- In-flight-операции, для сбора по истечению lease.
CREATE INDEX agent_operations_in_flight_lease
    ON agent_operations (lease_expires_at)
    WHERE status = 'IN_FLIGHT';

-- §9: одновременно на одну ноду отправляется не более одной mutating operation.
--
-- Индекс, а не проверка в запросе dispatcher'а: гейт «на этой ноде нет IN_FLIGHT»
-- читает committed-снимок, поэтому два воркера, взявшие в один момент РАЗНЫЕ
-- операции одной ноды, оба его проходят и оба коммитятся. Инвариант держится
-- только структурно; проигравший получает unique violation и просто пробует
-- позже.
--
-- Гейт по протухшему lease здесь не выражен намеренно: слот освобождает сборщик,
-- переводя операцию в RETRY_WAIT или SUPERSEDED, и он выполняется первым
-- оператором того же шага dispatcher'а.
CREATE UNIQUE INDEX agent_operations_single_in_flight_per_node
    ON agent_operations (node_id)
    WHERE status = 'IN_FLIGHT';
