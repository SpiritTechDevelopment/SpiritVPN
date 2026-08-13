#!/usr/bin/env bash
#
# Выкатка backend SpiritVPN на сервер среды.
#
#	./deploy.sh sha-a1b2c3d...
#
# Три шага в фиксированном порядке: схема накатывается раньше, чем поднимается
# процесс. Нарушение порядка под docker compose не ловится ничем — gRPC слушает
# независимо от /health/ready, и процесс на схеме старше встроенной начнёт
# принимать вызовы, роняя их на SQL-запросах. Ради этого порядка скрипт и
# существует.
#
# Простой даёт только третий шаг. Скачивание образов и накат схемы идут при
# работающем старом процессе: он остаётся готовым на схеме новее своей, потому
# что проверка требует версию не ниже встроенной, а не равную ей.

set -euo pipefail

cd "$(dirname "$0")"

tag=${1:?использование: deploy.sh <тег образа>}

if [ ! -f .env ]; then
	echo "deploy: рядом нет .env — скопируйте env.example и заполните" >&2
	exit 1
fi

registry=$(sed -n 's/^SPIRITVPN_REGISTRY=//p' .env)
if [ -z "$registry" ]; then
	echo "deploy: в .env не задан SPIRITVPN_REGISTRY" >&2
	exit 1
fi

# Тег фиксируется на диске: перезапуск сервера и `docker compose ps` должны
# работать без аргументов.
if grep -q '^SPIRITVPN_TAG=' .env; then
	sed -i "s|^SPIRITVPN_TAG=.*|SPIRITVPN_TAG=${tag}|" .env
else
	printf 'SPIRITVPN_TAG=%s\n' "$tag" >>.env
fi

echo "==> 1/3 образы ${tag}"
docker compose pull
docker pull "${registry}/spiritvpn-migrate:${tag}"

echo "==> 2/3 схема"
# Оба имени переменной подключения лежат в .env; migrate читает DATABASE_URL.
docker run --rm --env-file .env "${registry}/spiritvpn-migrate:${tag}" up

echo "==> 3/3 процесс"
docker compose up -d

# Бюджет ожидания готовности. Старт процесса ожиданий не содержит, но два
# потолка в нём есть: ping базы до 10 секунд и сама проверка готовности до 3.
readonly READY_TIMEOUT=30

echo "==> готовность"
for _ in $(seq "$READY_TIMEOUT"); do
	if docker compose exec -T spiritvpnd \
		wget -qO- http://127.0.0.1:8080/health/ready >/dev/null 2>&1; then
		echo "выкачено: ${tag}"
		exit 0
	fi
	sleep 1
done

echo "deploy: процесс не сообщил о готовности за ${READY_TIMEOUT} с" >&2
docker compose logs --tail 50 spiritvpnd >&2
exit 1
