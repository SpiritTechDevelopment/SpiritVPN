# Образы backend SpiritVPN: два runtime-таргета из одной сборки.
#
#	docker build --target migrate    -t spiritvpn/migrate:<тег> .
#	docker build --target spiritvpnd -t spiritvpn/spiritvpnd:<тег> .
#
# Оба тега обязаны приходить из одного коммита. Набор миграций встроен в бинарь
# (internal/migrations), и у него два потребителя: migrate их накатывает, а
# spiritvpnd из имён тех же файлов вычисляет версию схемы, которую требует его
# проверка готовности. Разъехавшиеся теги дадут под, застрявший в
# `not ready: schema`, причём ни одна миграция при этом не упадёт.
#
# spiritvpnd стоит последним, поэтому `docker build .` без --target собирает его.

FROM golang:1.26-alpine AS build

WORKDIR /src

# Зависимости отдельным слоем: правка исходников не должна тянуть повторный
# go mod download.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 даёт статические бинари. Без него сборка на golang:alpine
# линкуется с musl, и образ перестаёт переживать смену базы.
#
# trimpath убирает из бинаря пути сборочной машины: они попадают в трассы паник
# и в остальном бесполезны.
#
# Оба бинаря собираются здесь независимо от выбранного таргета. Стадия общая, и
# её слои переиспользуются вторым образом целиком.
RUN CGO_ENABLED=0 go build -trimpath -o /out/spiritvpnd ./cmd/spiritvpnd && \
    CGO_ENABLED=0 go build -trimpath -o /out/migrate ./cmd/migrate

# --- общий runtime -----------------------------------------------------------

FROM alpine:3.22 AS runtime

# ca-certificates нужен обеим программам при подключении к PostgreSQL с
# sslmode=verify-ca и verify-full: корни берутся системные. Оба mTLS-пути
# spiritvpnd свои CA читают из файлов и от системных корней не зависят.
RUN apk add --no-cache ca-certificates

# Непривилегированный пользователь. Ни одна из программ не пишет на диск и не
# открывает портов ниже 1024.
RUN adduser -S -u 10001 -H spiritvpn

USER 10001

# --- накат схемы -------------------------------------------------------------

FROM runtime AS migrate

COPY --from=build /out/migrate /usr/local/bin/migrate

# Параметры подключения этот образ берёт из DATABASE_URL либо из набора DB_*.
# Переменные SPIRIT_* он не читает.
ENTRYPOINT ["/usr/local/bin/migrate"]

# --- сам процесс -------------------------------------------------------------

FROM runtime AS spiritvpnd

COPY --from=build /out/spiritvpnd /usr/local/bin/spiritvpnd

# 8443 — gRPC под mTLS, его и публикуют. 8080 — служебный порт с /health/live,
# /health/ready и /metrics: TLS и проверки вызывающего у него нет, наружу он не
# выставляется.
EXPOSE 8443 8080

ENTRYPOINT ["/usr/local/bin/spiritvpnd"]
