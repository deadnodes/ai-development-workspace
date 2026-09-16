# Kubernetes: отдельный Control Plane на продукт

Поддерживаемая схема установки:

| | Product A | Product B |
| --- | --- | --- |
| Namespace | `rcp-product-a` | `rcp-product-b` |
| Service / Deployment | `release-control` | `release-control` |
| PostgreSQL | отдельная БД и пользователь A | отдельная БД и пользователь B |
| API token / GitHub App secrets | свои | свои |
| Состояние | только Product A | только Product B |

Один экземпляр обслуживает всех разработчиков, агентов и стенды своего продукта: dev/test/stage/prod не требуют отдельных Control Plane. Это способ установки, а не ограничение модели: несколько Products внутри экземпляра по-прежнему допустимы. В dedicated instance создай один Product. Между экземплярами нет общего реестра или синхронизации.

База содержит Deployment с одной репликой и ClusterIP Service. UI, API, MCP и worker работают в одном контейнере. PostgreSQL предоставляется отдельно — существующий сервер, managed DB или ваш PostgreSQL operator. Разные БД могут находиться на одном PostgreSQL сервере; не указывай экземплярам одну и ту же БД. Приложение создаёт таблицы при запуске; DB user должен иметь права DDL на свою схему. Резервное копирование обеспечивает оператор БД.

## 1. Собери и опубликуй образ

Из корня репозитория; замени registry и tag на свои:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/ORG/release-control:VERSION --push .
```

Сохрани опубликованный manifest digest. Готовый публичный образ пока не предполагается. Для private registry настрой `imagePullSecrets` в Deployment overlay.

## 2. Создай overlay продукта

В этом репозитории локальные overlays можно держать в игнорируемом `.local/`; для GitOps храни их в отдельном deployment-репозитории. Пример из корня исходников:

```sh
mkdir -p .local/kubernetes/product-a
cat > .local/kubernetes/product-a/kustomization.yaml <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: rcp-product-a
resources:
  - ../../../deploy/kubernetes
images:
  - name: release-control
    newName: ghcr.io/ORG/release-control
    digest: sha256:REPLACE_WITH_PUBLISHED_DIGEST
configMapGenerator:
  - name: release-control-config
    literals:
      - ALLOWED_HOSTS=release-control,release-control.rcp-product-a.svc,release-control.rcp-product-a.svc.cluster.local
YAML
```

Подставь registry/digest. Для другого продукта скопируй overlay и измени namespace, адреса и секреты. Все имена ресурсов могут совпадать: они изолированы namespace. Namespace не заменяет сетевые политики кластера или отдельные DB credentials.

## 3. Подключи БД и секреты

Выбери целевой kube-context явно. Не применяй манифесты в случайный текущий кластер.

```sh
export RCP_CONTEXT=YOUR_KUBE_CONTEXT
export RCP_NAMESPACE=rcp-product-a
kubectl --context "$RCP_CONTEXT" create namespace "$RCP_NAMESPACE"
```

Если namespace уже существует, используй его. Подготовь два локальных файла: `.local/database-url` с полным PostgreSQL URL и `.local/api-token` с непустым случайным токеном. Значения должны быть без завершающего перевода строки. Не помещай реальные значения в Git, инструкции или историю команд.

```sh
kubectl --context "$RCP_CONTEXT" -n "$RCP_NAMESPACE" create secret generic release-control-config \
  --from-file=DATABASE_URL=.local/database-url \
  --from-file=RC_TOKEN=.local/api-token
```

Для GitHub App (можно подключить после первого запуска):

```sh
kubectl --context "$RCP_CONTEXT" -n "$RCP_NAMESPACE" create secret generic release-control-github-app \
  --from-file=github-app.pem=/PRIVATE/PATH/github-app.pem
```

В connection Control Plane используй `private_key_ref: file:/run/secrets/github-app.pem`. App ID, Installation ID, repos и build/GitOps mapping настраиваются через API/MCP по [agent quickstart](AGENT_QUICKSTART.md). Kubernetes API permissions приложению не нужны: service-account token не монтируется. Подключение к Kubernetes здесь означает размещение самого сервиса; Flux/runtime observer от этого не появляется.

Secret/config updates: обновляй существующий Secret вашим secret manager или обычным Kubernetes workflow. После смены DB URL/API token перезапусти Deployment; переменные окружения не обновляются в работающем процессе. PEM volume обновляется Kubernetes, но уже выпущенный installation token может оставаться в memory cache до истечения; для немедленного сброса также перезапусти Deployment.

## 4. Применение и доступ

```sh
kubectl kustomize .local/kubernetes/product-a
kubectl --context "$RCP_CONTEXT" apply -k .local/kubernetes/product-a
kubectl --context "$RCP_CONTEXT" -n "$RCP_NAMESPACE" rollout status deployment/release-control
kubectl --context "$RCP_CONTEXT" -n "$RCP_NAMESPACE" port-forward service/release-control 8090:80
```

Последняя команда работает, пока нужен локальный доступ. UI: `http://127.0.0.1:8090`. MCP: `http://127.0.0.1:8090/mcp`, Streamable HTTP, заголовок `Authorization: Bearer <RC_TOKEN>`. UI принимает token через Identity & access.

Агент внутри кластера: `http://release-control.rcp-product-a.svc/mcp`, тот же bearer token. Для постоянного внешнего доступа добавь Ingress вашей платформы с TLS и включи его hostname в `ALLOWED_HOSTS`. Сохраняй Host и Authorization headers. Один hostname обслуживает `/`, `/api/*`, `/mcp`; не переписывай префикс. Настрой proxy для Streamable HTTP/SSE. Готовый Ingress не навязывается: ingressClass, домен и TLS secret зависят от платформы.

Создай один Product через MCP/API. Все агенты этого продукта используют один URL и получают контекст из одной БД. Shared token даёт доступ ко всему экземпляру; granular RBAC пока нет.

## 5. Обновление и диагностика

Обнови digest в overlay и повтори `apply -k`. Стратегия Recreate допускает короткий простой, избегая одновременного запуска разных версий. Состояние и операции остаются в PostgreSQL. App не требует PVC. Удаление namespace удалит его Secrets, но сохранность внешней БД зависит от вашего DB lifecycle.

```sh
kubectl --context "$RCP_CONTEXT" -n "$RCP_NAMESPACE" logs deployment/release-control --tail=100
kubectl --context "$RCP_CONTEXT" -n "$RCP_NAMESPACE" describe deployment release-control
kubectl --context "$RCP_CONTEXT" -n "$RCP_NAMESPACE" rollout restart deployment/release-control
```

Probes проверяют `/healthz` с `Host: localhost`, чтобы пройти host allowlist. Это HTTP/process health после начального подключения к БД, не непрерывная проверка PostgreSQL или worker progress. Проверяй операции через `get_operation` и `get_attention_required`. Непустой API token обязателен по этой инструкции; отсутствие ключа в Secret блокирует старт, но пустое значение не валидируется сервером.

Манифесты можно рендерить офлайн через `kubectl kustomize deploy/kubernetes`. Реальный rollout требует вашего image, БД, credentials и выбранного кластера; успешный render не равен успешному deployment.

Справка: [Kustomize](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/kustomization/), [HTTP probes и Host header](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/).

## Persistent project configuration

Project configuration is loaded from PostgreSQL, not from the Kubernetes overlay. The overlay only bootstraps the service and its DB/secret connectivity. Replacing the Pod preserves all configuration/history when the same database is retained. Git mirrors are optional one-way exports: [configuration authority](CONFIGURATION_AUTHORITY.md).

Для переноса на другую установку используй [полный gzip backup/restore](BACKUP.md) через два port-forward. Секреты и подключение к БД создаются отдельно; целевая application DB должна быть пустой.
