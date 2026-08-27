-- Expiry worker.
--
-- Записей у него своих нет: он переиспользует запросы командного пути и
-- материализации. Ниже единственное, чего у них не было, — выборка due customer.

-- Берёт одного истёкшего customer, у которого ещё осталось что снимать.
--
-- FOR UPDATE SKIP LOCKED здесь и есть вся координация: ни джобы, ни lease, ни
-- курсора воркеру не нужно. Занятого customer вторая реплика не ждёт,
-- а сразу берёт следующего; повторный запуск безопасен по построению.
--
-- Условие на PRESENT-access обязательно, и не ради аккуратности: без него уже
-- погашенный customer оставался бы due навсегда, воркер строил бы по нему пустые
-- планы и никогда не сообщал бы о простое — то есть крутился бы вхолостую на
-- полной скорости цикла. Оно же закрывает требование «повторный запуск и
-- несколько worker replicas не создают неограниченного числа Remove operations».
--
-- Ретайрнутые access в условие не входят: они уже ABSENT, и снимать с них нечего.
--
-- Строка возвращается целиком, а не одним customer_id: expires_at из неё
-- перечитывается под locком и задаёт план, чтобы expiry не снёс доступ после уже
-- закоммиченного renewal.
-- name: LockNextDueExpiredCustomer :one
SELECT customer_id, vpn_fleet_id, expires_at, desired_version, last_command_number,
       created_at, updated_at
FROM customer_entitlements e
WHERE e.expires_at <= now()
  AND e.lifecycle_state = 'ACTIVE'
  AND EXISTS (
      SELECT 1
      FROM vpn_accesses a
      WHERE a.customer_id = e.customer_id
        AND a.desired_state = 'PRESENT'
        AND a.retired_at IS NULL
  )
ORDER BY e.expires_at, e.customer_id
FOR UPDATE SKIP LOCKED
LIMIT 1;
