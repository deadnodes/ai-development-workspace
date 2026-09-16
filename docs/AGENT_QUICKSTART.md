# Инструкция для агента

Цель: запустить Control Plane, получить контекст работы через MCP и подключить GitHub → Actions → GHCR → DEV GitOps. Все действия доступны без UI.

Для выделенного сервиса на продукт в Kubernetes используй [KUBERNETES.md](KUBERNETES.md). MCP/API и команды настройки те же.

## 1. Запуск

Из корня репозитория. Нужен только Docker с Compose:

```sh
docker compose --profile app up --build -d
curl --fail http://127.0.0.1:8090/healthz
```

Адреса: UI `http://127.0.0.1:8090`, API `/api/*`, MCP `/mcp`.

```sh
docker compose logs --tail=100 app   # диагностика
# После изменения кода:
docker compose --profile app up --build -d
# Остановка с сохранением БД:
docker compose --profile app down
```

Данные находятся в PostgreSQL named volume. Не используй `down -v`: это удалит состояние.

## 2. Подключение агента

В MCP-клиенте добавь сервер:

```text
name: release-control
transport: Streamable HTTP
url: http://127.0.0.1:8090/mcp
```

URL должен быть доступен из процесса агента: его `127.0.0.1` может относиться к другой машине/контейнеру. Для удалённого агента используй SSH-туннель к машине Control Plane. Если сервер настроен с `RC_TOKEN`, передавай `Authorization: Bearer <token>`. Стандартный Compose запускает локальный сервер без токена; переменная из shell автоматически в контейнер не передаётся.

Первый вызов — `get_state {}`. Найди нужную Feature и вызови `resume {"feature_id":"…"}`. Не предполагай фиксированные Product/Feature ID. На чистой БД сначала создай Product, Feature и Integrations.

Основные MCP-вызовы:

| Задача | Tool и аргументы |
| --- | --- |
| Восстановить контекст | `resume {"feature_id":"…"}` |
| Контекст интеграции и внешние зависимости | `get_integration_context {"integration_id":"…"}` |
| Последнее наблюдение Git | `get_git_state {"integration_id":"…"}` |
| Состояние стенда | `get_environment_state {"environment_id":"…"}` |
| Записать действие | `execute {"action":"…","actor":"agent/<name>","data":{…}}` |
| Запросить DEV deployment | `deploy_integration {"actor":"agent/<name>","integration_id":"…","environment_id":"…"}` |
| Проверить операцию | `get_operation {"operation_id":"…"}` |
| Узнать, что требует внимания | `get_attention_required {"product_id":"…"}` |

Git state — сохранённое наблюдение. Для обновления сначала `execute` с `action: refresh_integration_git`, верхнеуровневым `integration_id` и `data: {}`; затем опрашивай возвращённую операцию.

Перед остановкой запиши handoff:

```json
{"action":"handoff","actor":"agent/backend","feature_id":"FEATURE_ID","integration_id":"INTEGRATION_ID","data":{"completed":["Что завершено"],"current":"Где остановился","remaining":["Что осталось"],"next":["Следующий конкретный шаг"],"warnings":["Что нельзя потерять"]}}
```

## 3. API вместо MCP

Схема команд: `GET /api/schema`. Состояние: `GET /api/state`. Контекст: `GET /api/features/{id}/context`.

Тот же JSON, что принимает MCP `execute`, отправляется в `POST /api/commands`:

```sh
curl --fail-with-body http://127.0.0.1:8090/api/commands \
  -H 'Content-Type: application/json' \
  --data-binary @- <<'JSON'
{"action":"create_product","actor":"agent/setup","data":{"name":"Example","description":"Managed product"}}
JSON
```

Сохраняй возвращённый `id`. Сначала читай состояние: повторная настройка не должна создавать дубликаты. Полный набор полей — [CONTRACT.md](CONTRACT.md) и `/api/schema`.

## 4. Подключение реального GitHub

Нужны: GitHub App ID, Installation ID, путь к RSA PEM; SOURCE repo + существующая feature branch; GITOPS repo + DEV файл; GHCR image name. Не угадывай эти значения.

App устанавливается на SOURCE и GITOPS репозитории с Metadata read, Contents read/write, Actions read/write. Используй отдельный App. Авторизованная `gh`-сессия помогает исследовать репозитории, но сервер её не использует.

PEM положи вне Git в `.secrets/github-app.pem` или каталог `RCP_SECRET_DIR_HOST`. Compose монтирует его в `/run/secrets`. Обеспечь чтение файла контейнерным UID 10001, не открывая ключ всем пользователям. В API передавай только `file:/run/secrets/github-app.pem`, никогда содержимое ключа.

Выполни команды ниже через `execute` или `/api/commands`. Подставь реальные значения вместо заглавных маркеров; `APP_ID` и `INSTALLATION_ID` — числа. Каждая строка — отдельная команда; сохраняй её ответ.

1. `create_product` → `PRODUCT_ID` (или выбери существующий).
2. Создай instance connection → `CONNECTION_ID`:

```json
{"action":"create_github_connection","actor":"agent/setup","data":{"name":"GitHub","app_id":APP_ID,"installation_id":INSTALLATION_ID,"owner":"ORG","private_key_ref":"file:/run/secrets/github-app.pem","api_url":"https://api.github.com"}}
```

3. Проверь connection и обнаружь репозитории через HTTP:

```sh
curl --fail-with-body -X POST http://127.0.0.1:8090/api/connections/CONNECTION_ID/test
curl --fail-with-body http://127.0.0.1:8090/api/connections/CONNECTION_ID/repositories
```

Discovery заполняет общий Registry числовыми GitHub ID. Затем привяжи connection и выбранные repos к Product:

```json
{"action":"grant_connection","actor":"agent/setup","product_id":"PRODUCT_ID","data":{"connection_id":"CONNECTION_ID"}}
{"action":"import_repository","actor":"agent/setup","product_id":"PRODUCT_ID","data":{"connection_id":"CONNECTION_ID","full_name":"ORG/SOURCE","role":"SOURCE","default_branch":"main","base_branch":"main"}}
{"action":"import_repository","actor":"agent/setup","product_id":"PRODUCT_ID","data":{"connection_id":"CONNECTION_ID","full_name":"ORG/GITOPS","role":"GITOPS","default_branch":"main","base_branch":"main"}}
```

Последние два ответа дают `SOURCE_ID` и `GITOPS_ID` — ID привязок Product. Именно их используй дальше, не числовые GitHub ID. Имена веток возьми из discovery. Один зарегистрированный repo может быть выбран несколькими Products.

4. Создай Component, Feature, Integration и Environment:

```json
{"action":"create_application","actor":"agent/setup","product_id":"PRODUCT_ID","data":{"name":"backend","repository_id":"SOURCE_ID"}}
{"action":"create_feature","actor":"agent/setup","product_id":"PRODUCT_ID","data":{"title":"First DEV integration","problem":"Describe the problem","goal":"Describe the intended outcome","repositories":["SOURCE_ID"]}}
{"action":"create_integration","actor":"agent/setup","feature_id":"FEATURE_ID","data":{"title":"First change","objective":"Describe the change","repositories":["SOURCE_ID"],"branches":[{"repository_id":"SOURCE_ID","name":"FEATURE_BRANCH"}]}}
{"action":"create_environment","actor":"agent/setup","product_id":"PRODUCT_ID","data":{"name":"dev"}}
```

Сохрани `APPLICATION_ID`, `FEATURE_ID`, `INTEGRATION_ID`, `ENVIRONMENT_ID`. Component в текущем API называется Application. Для существующей интеграции используй `update_integration`. Feature выбирает подмножество repos Product, Integration — подмножество Feature.

5. В SOURCE repo установи build-only workflow: скопируй `examples/github-actions/build-image.yml` и `registry_contract.py` в `.github/workflows/`. Проверь Dockerfile/build context. Workflow должен быть зарегистрирован на default branch. Задай repository variable `RCP_IMAGE_REPOSITORY=ghcr.io/org/image`; дай workflow доступ к GHCR package. Подробности контракта — [GITHUB_DEV.md](GITHUB_DEV.md).

```json
{"action":"configure_component","actor":"agent/setup","product_id":"PRODUCT_ID","data":{"application_id":"APPLICATION_ID","connection_id":"CONNECTION_ID","workflow":"build-image.yml","workflow_ref":"main","image_repository":"ghcr.io/org/image","rebuild_missing":true}}
```

6. Укажи существующий DEV файл в GITOPS. Простой поддерживаемый формат:

```yaml
image:
  repository: ghcr.io/org/image
  digest: sha256:ACTUAL_DIGEST
```

```json
{"action":"configure_environment","actor":"agent/setup","product_id":"PRODUCT_ID","data":{"environment_id":"ENVIRONMENT_ID","application_id":"APPLICATION_ID","connection_id":"CONNECTION_ID","repository_id":"GITOPS_ID","purpose":"DEV","ref":"main","path":"environments/dev/backend.yaml","image_field":"image.repository","digest_field":"image.digest","allow_deploy":true}}
```

Этот файл должен реально использоваться вашим GitOps/Flux pipeline. Для HelmRelease с chart, использующим image tag, используются `image_field: spec.values.image.repository` и `digest_field: spec.values.image.tag`; подробности поддерживаемого формата — в [GITHUB_DEV.md](GITHUB_DEV.md). Не добавляй неиспользуемый digest в chart.

## 5. Первый DEV-прогон

Вызови `deploy_integration` и сохрани ID возвращённой операции. Опрашивай `get_operation` примерно раз в 5–10 секунд до `SUCCEEDED`, `FAILED` или `CANCELLED`. Не отправляй повторный deploy вместо ожидания.

Проверь evidence: source SHA → Actions run → GHCR digest → GitOps commit. `SUCCEEDED` с `GITOPS_APPLIED` означает, что desired state записан. Это **не** подтверждение работающих pods: Flux/runtime observation пока не реализована.

При ошибке читай шаг/error и `get_attention_required`. Исправь конкретную причину. BEHIND/DIVERGED блокируют deploy; реальный SOURCE branch надо обновить отдельно. Missing artifact запускает CI только при `rebuild_missing: true`. GitOps изменяется только при `allow_deploy: true`.

## 6. Работа над самим Control Plane

Контекст и handoff хранятся в приложении; `IMPLEMENTATION_STATE.md` — только recovery locator. Архитектура: [ARCHITECTURE.md](ARCHITECTURE.md). Перед сдачей изменений кода:

```sh
TEST_DATABASE_URL='postgres://releasecontrol:releasecontrol@localhost:55432/releasecontrol?sslmode=disable' make check
```

Для этой команды нужны Go 1.25+, Node.js и Python 3, а также запущенный Compose db. UI-изменения дополнительно проверь в браузере. Реальные GitHub тесты opt-in; не объявляй живой DEV-прогон успешным по результатам fake-provider тестов.
