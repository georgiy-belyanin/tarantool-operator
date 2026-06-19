# tarantool3-operator

Deploys the Kubernetes operator for Tarantool 3 clusters
(`db.tarantool.io/v2alpha1`: `Cluster`, `ReplicaSet`).

## Install

```sh
helm install tarantool-operator ./charts/tarantool3-operator \
  --namespace tarantool-operator --create-namespace
```

Then describe a cluster with plain resources (see
[`config/samples/tarantool3`](../../../config/samples/tarantool3) in the
operator repository):

```yaml
apiVersion: db.tarantool.io/v2alpha1
kind: Cluster
metadata:
  name: example
spec: {}
---
apiVersion: db.tarantool.io/v2alpha1
kind: ReplicaSet
metadata:
  name: example-rs
spec:
  clusterName: example
  replicas: 3
  podTemplate:
    spec:
      containers:
        - name: tarantool
          image: tarantool/tarantool:3
```

The operator provisions a built-in `tarantool-operator` Tarantool user (role
`super`, generated password in the `<cluster>-operator-user` Secret), so leader
observation, configuration hot reload, schema upgrades and drain-gated
scale-in work without manual credentials setup. Opt out per cluster with
`spec.disableOperatorUser: true`.

## CRDs

CRDs live in `crds/` (synced from `config/crd/bases/tarantool3` in the
operator repository). Helm installs them on first `helm install` but does NOT
upgrade them; apply CRD updates explicitly when upgrading the operator:

```sh
kubectl apply -f charts/tarantool3-operator/crds/
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `tarantool/tarantool-operator` | Operator image |
| `image.tag` | chart `appVersion` | Image tag |
| `replicas` | `2` | Manager replicas (leader election keeps one active) |
| `ports.health` / `ports.metrics` | `8081` / `8080` | Probe and metrics ports |
| `resources` | small requests | Manager container resources |
| `serviceAccount.create` / `serviceAccount.name` | `true` / generated | ServiceAccount wiring |
| `rbac.create` | `true` | ClusterRole/Bindings + leader-election Role |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName` | empty | Scheduling |
| `podLabels`, `podAnnotations`, `labels`, `annotations` | empty | Extra metadata |

Note: do not install this chart next to the legacy `tarantool-operator` chart
in the same cluster — the binary also contains the legacy Cartridge
controllers, and two deployments would manage the same `tarantool.io`
resources.
