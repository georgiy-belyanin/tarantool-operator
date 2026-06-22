# RFC: Kubernetes-оператор Tarantool 3

| | |
|---|---|
| Статус | Draft |
| API | `db.tarantool.io/v2alpha1` |

## 1. Введение

Оператор разворачивает и сопровождает кластеры Tarantool 3 в Kubernetes на
нативной декларативной конфигурации Tarantool — без Cartridge. Пользователь
описывает кластер двумя ресурсами (`Cluster` и `ReplicaSet`); оператор генерирует
из них конфигурацию Tarantool, доставляет её в поды (файлом из Secret) и приводит
кластер к заданному состоянию, дальше доводя его до сходимости.

Основная цель — Community Edition: доставка конфигурации локальным файлом, без
обязательной зависимости от внешнего хранилища. Конфигурация описывается в
терминах самого Tarantool, а не в операторских абстракциях, — оператор берёт на
себя только то, что обязан вычислить сам (топология, имена и адреса инстансов,
bootstrap, пароли) и операции дня-2 (масштабирование, перезагрузка конфигурации,
обновления, восстановление).

## 2. Типы ресурсов

**`Cluster`** — общая для всего кластера часть конфигурации: домен,
`credentialsSecret`, секция `config` (global scope) и `groupConfigs`
(конфигурация на уровне group). Сам по себе подов не создаёт.

**`ReplicaSet`** — один репликасет Tarantool. Соответствует **одному
StatefulSet** (один к одному); поды StatefulSet — это инстансы
(`<replicaset>-0`, `-1`, …). Шардированный слой описывается несколькими
`ReplicaSet`. Ссылается на `Cluster` через `clusterName` и относится к группе
`group`.

```
Cluster                      → Secret (config.yaml) + headless Service
  └─ ReplicaSet (group=…)    → StatefulSet
       └─ pod <rs>-<ordinal> → инстанс Tarantool
```

### Конфигурация в терминах Tarantool

В ресурсах нет операторских аналогов опций Tarantool. Любая настройка задаётся
как конфигурация Tarantool в сквозных (passthrough) секциях; оператор лишь
распределяет их по scope и подставляет значения, которые обязан вычислить сам:
имена инстансов, `iproto.advertise`/`listen`, bootstrap leader, пароли.

| Scope | Где задаётся |
|---|---|
| global | `Cluster.spec.config` |
| group | `Cluster.spec.groupConfigs.<group>` |
| replicaset | `ReplicaSet.spec.config` |
| instance | `ReplicaSet.spec.instanceConfigs.<ordinal>` |

Приоритет — как в Tarantool: instance → replicaset → group → global; map
объединяются, array заменяются целиком. Значения, вычисляемые оператором, имеют
приоритет над пользовательскими (например, заданный пользователем
`iproto.advertise.peer.uri` игнорируется). Если пользователь задал поле, которое
генерируется оператором, оператор сообщает об этом событием и в статусе.

### Пример: описание кластера

```yaml
apiVersion: db.tarantool.io/v2alpha1
kind: Cluster
metadata: {name: app}
spec:
  credentialsSecret: app-creds      # ключи Secret = имена пользователей
  config:
    replication: {failover: election}
    credentials:
      users:
        replicator: {roles: [replication, super]}
  groupConfigs:
    storages:                        # group scope
      memtx: {memory: 268435456}
---
apiVersion: db.tarantool.io/v2alpha1
kind: ReplicaSet
metadata: {name: storage-001}
spec:
  clusterName: app
  group: storages
  replicas: 3
  config:                           # replicaset scope
    sharding: {roles: [storage]}
  instanceConfigs:
    "0": {zone: a}                  # instance scope
  podTemplate:
    spec:
      containers:
      - {name: tarantool, image: tarantool/tarantool:3}
```

### Пример: сгенерированный по манифестам конфиг

Из манифестов выше оператор генерирует один документ конфигурации Tarantool
(монтируется в поды как `config.yaml`). Пароль `replicator` подставлен из Secret
`app-creds`; топология, адреса и bootstrap leader вычислены оператором:

```yaml
credentials:
  users:
    replicator:
      roles: [replication, super]
      password: s3cr3t            # из Secret app-creds (ключ replicator)
replication:
  failover: election
iproto:
  advertise:
    peer: {login: replicator}     # под каким пользователем ходит репликация
console:                          # для readiness-пробы (box.info.status)
  enabled: true
  socket: /var/run/tarantool/admin.socket
groups:
  storages:
    memtx: {memory: 268435456}    # из groupConfigs.storages
    replicasets:
      storage-001:
        sharding: {roles: [storage]}
        bootstrap_leader: storage-001-0          # вычислен оператором
        replication: {bootstrap_strategy: config}
        instances:
          storage-001-0:
            zone: a                              # из instanceConfigs."0"
            iproto:
              listen: [{uri: "0.0.0.0:3301"}]
              advertise: {peer: {uri: storage-001-0.app.default.svc.cluster.local:3301}}
          storage-001-1:
            iproto:
              listen: [{uri: "0.0.0.0:3301"}]
              advertise: {peer: {uri: storage-001-1.app.default.svc.cluster.local:3301}}
          storage-001-2:
            iproto:
              listen: [{uri: "0.0.0.0:3301"}]
              advertise: {peer: {uri: storage-001-2.app.default.svc.cluster.local:3301}}
```

## 3. Возможности оператора

Приоритет: P0 — корректность/доступность, P1 — значимый день-2, P2 — удобство.

| Группа | Возможность | Приоритет |
|---|---|---|
| **Конфигурация** | Tarantool-first config, scopes global/group/replicaset/instance | P0 |
| | Валидация по JSON-схеме Tarantool 3 | P0 |
| | Уведомление о ручной установке генерируемых полей | P2 |
| **Доставка конфигурации** | Файловая (Secret, смонтированный в поды) | P0 |
| | `etcd` / `config.storage` (watch + reload) | P1 |
| **Доставка секретов** | Kubernetes Secret (`credentialsSecret`) | P0 |
| | HashiCorp Vault | P1 |
| | Secrets Store CSI / external-secrets | P2 |
| **Топология** | Генерация из набора `ReplicaSet` / `replicas` | P0 |
| | Масштабирование (`kubectl scale`, без рестарта живых инстансов) | P0 |
| | Graceful expel при scale-down | P0 |
| **Шардирование (vshard)** | Bootstrap бакетов | P1 |
| | Rebalance при добавлении шарда | P1 |
| | Безопасное удаление шарда (drain весов) | P1 |
| **Проверки здоровья** | Readiness (`box.info.status`) | P0 |
| | Liveness / startup | P1 |
| **Конфигурация в рантайме** | Hot reload динамических ключей без рестарта | P1 |
| | Rolling update (`podTemplate`) | P1 |
| | Leader-aware упорядоченная накатка | P1 |
| | `box.schema.upgrade()` после смены образа | P1 |
| **Наблюдаемость** | `status` / `conditions` | P0 |
| | metrics Service + ServiceMonitor + alerts + dashboard | P1 |
| **Доступность узлов** | Восстановление при отказе узла (force-reschedule) | P1 |
| | PodDisruptionBudget | P1 |
| **Резервное копирование** | Backup / ScheduledBackup + PITR | P1 |
| **Доставка приложений** | Через образ / ConfigMap (`app.file`, `roles`) | P0 |
| | Абстракция доставки артефактов (RPM/rock) | P1 |

## 4. Поведение и примеры

Оператор декларативный и level-triggered: рендерит конфигурацию Tarantool из
обоих ресурсов, валидирует её по JSON-схеме Tarantool 3, кладёт в Secret (он же
несёт пароли), монтирует Secret в поды и указывает на него через
`TT_CONFIG`/`TT_INSTANCE_NAME`. Инстансы получают стабильные DNS-имена от
headless Service; их `iproto.advertise.peer.uri` совпадает с этими именами.

### 4.1. Топология и масштабирование

Топологию не описывает пользователь — она генерируется из набора `ReplicaSet` и
значений `replicas`: имена инстансов, иерархия `groups → replicasets →
instances`, адреса в `advertise`, детерминированный bootstrap leader (`<rs>-0`).
Каждый репликасет обязан иметь записываемый инстанс, иначе свежий кластер не
поднимется; оператор это обеспечивает (для `manual` задаёт `leader`, для `off` —
`database.mode: rw` на `<rs>-0`, для `election`/`supervised` лидера выбирает
Tarantool).

Масштабирование — штатными средствами Kubernetes:

```sh
kubectl scale ttrs storage-001 --replicas=5     # увеличение
kubectl scale ttrs storage-001 --replicas=3     # уменьшение
```

При увеличении новые инстансы появляются в конфигурации и присоединяются к
репликасету; существующие инстансы при этом не перезапускаются — новый состав
применяется к ним перезагрузкой конфигурации (при наличии `super`-пользователя,
см. §4.4). Без него смена состава перезапускает поды, иначе уцелевшие инстансы не
получат новый список пиров.

При уменьшении лишние инстансы удаляются из конфигурации и корректно исключаются
из кластера (`replication.autoexpel` по префиксу имени репликасета): при
перечитывании конфигурации лидер удаляет выбывшие инстансы из `_cluster`, не
оставляя устаревших записей в `box.info.replication`. Автоисключение действует
только после первичного формирования репликасета — во время параллельной
начальной загрузки оно конкурирует с протоколом присоединения и грозит
split-brain.

### 4.2. Шардирование (vshard)

Шард — отдельный `ReplicaSet` в storage-группе с ролью
`sharding.roles: [storage]`. Модуль `vshard` отсутствует в community-образе
`tarantool/tarantool:3`, поэтому нужен образ с vshard. Оператор раздаёт каждому
инстансу `iproto.advertise.sharding` на его DNS-имени.

- **Bootstrap бакетов** — `vshard.router.bootstrap()`.
- **Rebalance** при добавлении шарда выполняет сам vshard, как только storage с
  весом появляется в конфигурации; оператору достаточно её доставить.
- **Безопасное удаление шарда**: финалайзер `tarantool.io/drain-buckets`
  задерживает удаление — удаляемому репликасету проставляется `sharding.weight: 0`,
  vshard сливает его бакеты на оставшиеся storage, и только когда бакетов не
  осталось, финалайзер снимается и StatefulSet удаляется. Требуется
  `super`-пользователь.

#### Пример: добавление шарда

```yaml
# storage-002.yaml
apiVersion: db.tarantool.io/v2alpha1
kind: ReplicaSet
metadata: {name: storage-002}
spec:
  clusterName: app
  group: storages
  replicas: 3
  config:
    sharding: {roles: [storage], weight: 1}
  podTemplate:
    spec:
      containers:
      - {name: tarantool, image: tarantool-vshard:3}   # образ с vshard
```

```sh
kubectl apply -f storage-002.yaml
kubectl rollout status statefulset/storage-002        # дождаться готовности
# vshard сам перераспределит бакеты; прогресс видно на инстансе:
kubectl exec storage-002-0 -- sh -c \
  "echo 'return require(\"vshard\").storage.info().bucket.active' \
   | tt connect /var/run/tarantool/admin.socket"
```

#### Пример: безопасное удаление шарда

Обычный `kubectl delete`; финалайзер сольёт бакеты перед удалением:

```sh
kubectl delete ttrs storage-002              # вернётся сразу; удаление отложено
kubectl get ttrs storage-002 -o jsonpath='{.metadata.finalizers}{"\n"}'
#   ["tarantool.io/drain-buckets"]
kubectl get ttrs storage-002 -w              # дождаться исчезновения ресурса
kubectl get events --field-selector reason=ShardDrained
```

Если у удаляемого storage есть «приколотые» (pinned) бакеты, дренаж не
завершится — это сознательная защита от потери данных; снять финалайзер вручную
(`kubectl patch … --type=merge -p '{"metadata":{"finalizers":[]}}'`) можно только
осознанно.

### 4.3. Проверки Liveness / Readiness

**Readiness**: exec-проба читает `box.info.status` через локальный сокет
административной консоли; под готов только при значении `running` (а не когда
открылся порт). Сокет включается оператором (`console.enabled`) и не требует
аутентификации. Пользовательская проба, если задана, сохраняется.

**Liveness / startup**: проверка только живости процесса (без зависимости от
`box.info.ro`, чтобы не перезапускать исправные реплики) и startup-проба для
длительного восстановления из WAL.

### 4.4. Горячая перезагрузка конфигурации

Большинство опций Tarantool — динамические (`box.cfg`, `Dynamic: yes`), их не
нужно применять перезапуском.

- изменение только динамических опций (и числа инстансов) **не** перезапускает
  поды;
- при наличии `super`-пользователя оператор применяет такие изменения вызовом
  `config:reload()` по iproto на каждом инстансе и сверяет загруженную версию;
- перезапуск остаётся только для статических опций (`wal.dir`, `memtx.dir`,
  `wal.mode`, …) и для уменьшения `memtx.memory` (растёт только вверх — при
  уменьшении оператор предупреждает событием).

Файловая конфигурация не перечитывается автоматически при изменении файла —
`config:reload()` вызывается оператором явно. Альтернатива — источник
`config.storage`/etcd с собственным механизмом watch и перезагрузки.

#### Пример: правка динамической опции без рестарта

```sh
kubectl patch ttc app --type=merge \
  -p '{"spec":{"config":{"memtx":{"memory":536870912},"log":{"level":6}}}}'
# поды НЕ пересоздаются; новое значение становится живым на всех инстансах:
kubectl exec storage-001-0 -- sh -c \
  "echo 'return box.cfg.memtx_memory' | tt connect /var/run/tarantool/admin.socket"
#   - 536870912
```

#### Накатка обновлений (rolling update)

Изменение `podTemplate` (например, образа) перезапускает поды через StatefulSet.
Для `election` в поды добавляется хук `preStop` с `box.ctl.demote()` — лидер
слагает полномочия сразу, не дожидаясь таймаута выборов, что сокращает окно
недоступности записи. После смены образа оператор однократно выполняет
`box.schema.upgrade()` на лидере (при одинаковой версии на всех инстансах).

Leader-aware накатка: сначала реплики, лидер последним, по одному инстансу за раз,
с проверкой `box.info.replication[].upstream.status == follow` и сохранения
кворума на каждом шаге.

### 4.5. Управление секретами

Пароли пользователей хранятся в Kubernetes Secret, на который ссылается
`Cluster.spec.credentialsSecret` (ключ — имя пользователя, значение — пароль).
Оператор подставляет их в `credentials.users.<name>.password` при генерации
конфигурации, читая Secret только из пространства имён самого Cluster; в ресурсе
паролей нет. Создание и ротация Secret вызывают повторную генерацию конфигурации.

**HashiCorp Vault**: пароли можно брать из Vault. Источник включается аннотациями
кластера (без полей в CRD): `tarantool.io/vault-address`, `tarantool.io/vault-path`
(KV v2), `tarantool.io/vault-token-secret`. Пароли из Vault накладываются поверх
`credentialsSecret` (часть может быть в Secret, часть — в Vault). Интеграция через
Secrets Store CSI / external-secrets и Kubernetes-auth — отдельным шагом.

#### Пример: Secret с паролями

```yaml
apiVersion: v1
kind: Secret
metadata: {name: app-creds}      # ← Cluster.spec.credentialsSecret
stringData:
  replicator: s3cr3t             # ключ = имя пользователя из config.credentials.users
```

### 4.6. Наблюдаемость

Оба ресурса несут `status`: укрупнённую `phase`, счётчик `readyInstances`
(`готово/всего`), наблюдённого `leader` (по `box.info` через iproto) и
`conditions` (`Ready`, а для `ReplicaSet` — триаду
`Available`/`Progressing`/`Degraded`). Фаза `Degraded` отличается от
`Configuring`: она означает, что ранее полностью готовый репликасет потерял
инстансы. Оператор публикует и собственные метрики.

Метрики кластера: при включённых метриках оператор создаёт metrics-Service,
`ServiceMonitor`, `PrometheusRule` (нет лидера, потеря кворума, задержка
репликации, переполнение арены, дисбаланс бакетов) и дашборд.

### 4.7. Восстановление при отказе узла

При потере узла StatefulSet не пересоздаёт под самостоятельно (гарантия «не более
одного» с данной идентичностью), а том RWO не переносится — реплика остаётся
недоступной. По истечении grace-периода оператор принудительно удаляет зависший
под (узел недоступен либо под застрял в Terminating), после чего StatefulSet
пересоздаёт инстанс, и тот восстанавливает данные с лидера. Защита от
split-brain: только многоинстансные сформированные репликасеты; большинство
инстансов в состоянии ready; никогда не последний и не объявленный записываемым
инстанс.

Для каждого многоинстансного репликасета оператор создаёт `PodDisruptionBudget`
(`maxUnavailable=1`), чтобы плановые операции (drain узла, накатка) не уводили
кворум; для одноинстансного PDB не создаётся.

### 4.8. Резервное копирование

Ресурсы `Backup`/`ScheduledBackup` поверх `box.backup` с политикой хранения;
восстановление на момент времени (Point-In-Time Recovery).

### 4.9. Доставка приложений

Приложение и роли поставляются через собственный образ или смонтированный
ConfigMap (`app.file` либо `roles` + `LUA_PATH`). Отдельным шагом — абстракция для
доставки артефактов (RPM/rock) и сборки образа.

---

MVP: https://github.com/georgiy-belyanin/tarantool-operator (ветка
`tarantool-3-migration`).
