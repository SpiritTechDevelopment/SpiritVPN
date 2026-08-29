# VLESS URI

Backend отдаёт VLESS URI через gRPC-метод `GetCustomerAccessLinks`. Метод
возвращает все текущие access пользователя в поле `links`; у каждого элемента
есть вид доступа `kind` и состояние `state`. Поле `uri` присутствует только у
ссылки в состоянии `READY`. Полный контракт состояний, `block_reason` и порядок
элементов описан в [документации внешнего API](API.md).

## Выдача URI

Use case `GetCustomerAccessLinks.Execute` читает данные доступа и обрабатывает
каждую ссылку отдельно (`internal/app/get_customer_access_links.go:39`). Для
`READY` он расшифровывает `client_uuid`, берёт публичные параметры входной ноды
из текущей проекции манифеста и вызывает `BuildVLESSURI`
(`internal/app/get_customer_access_links.go:61`). Ошибка credential или
непригодные параметры ноды переводят только эту ссылку в `FAILED`; остальные
элементы ответа сохраняются.

Готовая URI собирается заново для каждого ответа и в базе не хранится. Изменение
публичного адреса или параметров REALITY начинает действовать при следующем
вызове метода. Ответ получает gRPC-метадату `cache-control: no-store`
(`internal/grpcsvc/customer_access.go:93`).

## Формат

Единственный сборщик находится в `internal/app/vless.go:32`. Он формирует URI
следующего вида:

```text
vless://<uuid>@<address>:<port>?security=reality&encryption=none&pbk=<public-key>&fp=<fingerprint>&type=<transport>&flow=<flow>&sni=<server-name>&sid=<short-id>#<display-name>
```

Например:

```text
vless://f81d4fae-7dec-11d0-a765-00a0c91e6bf6@nl.example.com:443?security=reality&encryption=none&pbk=pub-key&fp=chrome&type=tcp&flow=xtls-rprx-vision&sni=www.example.org&sid=ab12#Netherlands
```

Для XHTTP после `type=xhttp` добавляются `path` и `mode`, а `flow` исчезает:

```text
vless://f81d4fae-7dec-11d0-a765-00a0c91e6bf6@nl.example.com:443?security=reality&encryption=none&pbk=pub-key&fp=firefox&type=xhttp&path=%2Fapi%2Fv1%2Fconnect&mode=packet-up&sni=www.example.org&sid=ab12#Netherlands
```

| часть URI | значение | источник |
|---|---|---|
| схема | `vless` | константа протокола v1 |
| `uuid` | credential VLESS | расшифрованный `client_uuid` доступа |
| `address` | домен, IPv4 или IPv6 входной ноды | `NodePublic.Address` |
| `port` | публичный порт входной ноды | `NodePublic.Port` |
| `security` | `reality` | константа протокола v1 |
| `encryption` | `none` | константа протокола v1 |
| `pbk` | публичный ключ REALITY | `NodePublic.RealityPublicKey` |
| `fp` | fingerprint клиента | `NodePublic.Fingerprint` |
| `type` | `tcp` либо `xhttp` | `NodePublic.Transport` |
| `path` | путь XHTTP; присутствует только для XHTTP | `NodePublic.XHTTP.Path` |
| `mode` | режим XHTTP; присутствует только для XHTTP | `NodePublic.XHTTP.Mode` |
| `flow` | `xtls-rprx-vision`; отсутствует для XHTTP | `NodePublic.Flow` |
| `sni` | server name REALITY | `NodePublic.ServerName` |
| `sid` | short ID REALITY | `NodePublic.ShortID` |
| `display-name` | имя профиля у пользователя | `NodePublic.DisplayName` для `FREEDOM`, имя связи для `BRIDGE` |

Порядок query-параметров фиксирован в `vlessQuery`
(`internal/app/vless.go:57`): `security`, `encryption`, `pbk`, `fp`, `type`,
`path`, `mode` (только XHTTP), `flow`, `sni`, `sid`. Параметры с пустыми значениями остаются в строке. В
частности, допустимый пустой short ID имеет форму `sid=`.

Единственное исключение — `flow`: у XHTTP-ноды его нет вовсе, и параметр
опускается целиком, а не пишется пустым. Ссылка повторяет конфигурацию ноды
буквально; на форму TCP-ссылки это не влияет, там `flow` непуст всегда.

Сборщик кодирует значения query и фрагмент средствами `net/url`. Имя профиля с
пробелами или кириллицей становится percent-encoded. Хост собирается через
`net.JoinHostPort`, и IPv6-адрес записывается в квадратных скобках. Эти свойства
закреплены тестами в `internal/app/vless_test.go`.

## Пригодность параметров

Перед сборкой `NodePublic.Usable` проверяет адрес, порт в диапазоне 1..65535,
публичный ключ REALITY, server name, fingerprint и transport
(`internal/domain/node.go:59`). Пустые `ShortID` и `DisplayName` разрешены. Flow
обязателен только у TCP: у XHTTP-ноды его нет, и требовать его значило бы
объявить такую ноду сломанной. Для XHTTP вместо него обязательны `path` и `mode`.

Приём манифеста выполняет более строгую проверку: v1 принимает только `tcp`, а
v2 — `tcp` и `xhttp`; flow сверяется с транспортом, а fingerprint должен соответствовать
`^[A-Za-z0-9._-]{1,64}$` (`internal/domain/manifest.go:208`). Read-путь повторно
эти ограничения не применяет. Уже записанная нода продолжает участвовать в
выдаче URI, если её данных достаточно по `NodePublic.Usable`. Все поля
`public`, их проверки и последствия ошибок перечислены в
[документации манифеста](MANIFEST.md).

## Секреты

UUID находится в URI в открытом виде. После `BuildVLESSURI` это обычная строка,
на которую не распространяется защита типа `crypto.ClientUUID`. URI нельзя
писать в логи, трассировки или кеши. Серверный interceptor не логирует тело
ответа; это закрепляет `TestLoggingNeverRecordsResponseBody`
(`internal/grpcsvc/interceptors_test.go:401`).
