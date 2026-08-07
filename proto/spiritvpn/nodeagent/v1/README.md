# nodeagent/v1 — вендоренная замороженная копия

`node_agent.proto` — **байт-в-байт вендоренная копия** контракта backend↔node-agent,
которым владеет инфраструктурный репозиторий:

    источник: ../infra_v1/contracts/nodeagent/v1/node_agent.proto

Это источник истины для «провода» между backend и node-agent. **Не редактировать
руками здесь.** Для обновления — перевендорить из infra и отревьюить diff.

Заметки для реализующих backend:

- Поле `User.egress_key` (поле 4) уже присутствует в этом baseline, поэтому
  «pending change-request» из спеки (§9) этой копией уже удовлетворён.
- Inline-скетчи proto в `BACKEND_DOMAIN_AGREEMENTS.md` §9/§10/§12 —
  иллюстративные и намеренно проще этого контракта. Этот файл управляет «проводом»,
  а спека — внутренней семантикой backend. В частности,
  `desired_revision`/`applied_revision` — внутренние поля backend (см.
  `vpn_nodes.desired_revision`), а не wire-поля: на проводе идемпотентность — это
  `operation_id`, а полнота набора — `complete=true` + `users_complete` в ответе.

`go_package` внутри файла указывает на infra-модуль; при генерации backend
переопределяет его через buf managed mode (`go_package_prefix`), поэтому сам
вендоренный файл остаётся нетронутым.
