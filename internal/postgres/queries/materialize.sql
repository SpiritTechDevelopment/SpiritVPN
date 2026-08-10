-- Воркер материализации манифеста (§6, §13).
--
-- Чтения состояния customer здесь не дублируются: они те же, что у
-- ApplyCustomerAccess, и берутся из customer_entitlements.sql, quota_periods.sql,
-- node_quota_usage.sql, topology.sql и vpn_accesses.sql. Ниже только то, чего у
-- командного пути нет: координация джобы и записи, которых Apply не делает.

-- Берёт в работу самую старую незавершённую джобу (§13).
--
-- SKIP LOCKED, чтобы вторая реплика не ждала на уже занятой строке, а сразу
-- взяла следующую. Условие на lease разрешает три случая: джоба ещё ничья,
-- чужой lease протух (воркер упал — §13 требует восстановления), либо lease наш
-- собственный. Последнее обязательно: каждый шаг ProcessNext идёт отдельной
-- транзакцией, и без него воркер не смог бы продолжить собственную джобу.
-- name: ClaimMaterializationJob :one
UPDATE manifest_materialization_jobs
SET status           = 'IN_PROGRESS',
    lease_owner      = @owner::text,
    lease_expires_at = now() + make_interval(secs => @lease_seconds::int),
    updated_at       = now()
WHERE revision = (
    SELECT revision
    FROM manifest_materialization_jobs
    WHERE status IN ('PENDING', 'IN_PROGRESS')
      AND (lease_expires_at IS NULL
           OR lease_expires_at < now()
           OR lease_owner = @owner::text)
    ORDER BY revision
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING revision, coalesce(cursor->>'customer_id', '')::text AS cursor_customer_id;

-- Следующий customer в порядке customer_id. Ноль строк означает, что обход
-- завершён.
--
-- Обходятся ВСЕ customer, а не только затронутые манифестом: удаления по
-- manifest_revision не находятся — у ретайрнутых строк остаётся прежняя revision
-- (решения 23 и 29).
-- name: NextCustomerAfter :one
SELECT customer_id
FROM customer_entitlements
WHERE customer_id > @after_customer_id::text
ORDER BY customer_id
LIMIT 1;

-- Курсор двигается в той же транзакции, что и изменения customer (решение 34).
-- name: AdvanceMaterializationCursor :exec
UPDATE manifest_materialization_jobs
SET cursor     = jsonb_build_object('customer_id', @customer_id::text),
    updated_at = now()
WHERE revision = @revision;

-- Обход дошёл до конца. Lease снимается: перезапускать джобу больше не нужно.
-- name: CompleteMaterializationJob :exec
UPDATE manifest_materialization_jobs
SET status           = 'DONE',
    lease_owner      = NULL,
    lease_expires_at = NULL,
    updated_at       = now()
WHERE revision = @revision;

-- Двигает только desired_version корневой строки.
--
-- Отдельный запрос, а не UpdateCustomerEntitlement: тот пишет ещё expires_at и
-- last_command_number, а материализация не является командой product-сервиса и
-- не имеет права трогать ни срок, ни счётчик команд (§5).
-- name: BumpEntitlementDesiredVersion :exec
UPDATE customer_entitlements
SET desired_version = @desired_version,
    updated_at      = now()
WHERE customer_id = @customer_id::text;

-- Смена egress_tag связи при неизменных routing_key и паре (§6).
--
-- client_uuid, accounting_id и generation сохраняются: это repoint, а не новое
-- поколение. apply_state сбрасывается в PENDING — агенту предстоит переиздать
-- персональное правило по новому egress_key.
-- name: RepointAccess :exec
UPDATE vpn_accesses
SET egress_key      = @egress_key::text,
    desired_state   = @desired_state::text,
    desired_version = @desired_version,
    apply_state     = 'PENDING'
WHERE access_id = @access_id;

-- Цель исчезла из манифеста: access ретайрится и переводится в ABSENT (§6, §13).
--
-- Строка не удаляется никогда — по ней приходит поздний traffic, а повторное
-- появление цели создаёт новое поколение (§4, §11).
--
-- apply_state приходит параметром, потому что зависит от того, доставляется ли
-- удаление: на живую ноду выпускается EnsureUserAbsent и состояние PENDING, а на
-- глобально удалённую доставлять нечего, и она сразу APPLIED — иначе операция
-- висела бы недоставленной вечно (§6, решение 6).
-- name: RetireAccess :exec
UPDATE vpn_accesses
SET retired_at      = now(),
    desired_state   = 'ABSENT',
    desired_version = @desired_version,
    apply_state     = @apply_state::text
WHERE access_id = @access_id;
