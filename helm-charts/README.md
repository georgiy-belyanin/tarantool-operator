# Tarantool Helm charts

Helm charts for the Tarantool operator and applications.

## Charts

| Chart                                                | Status     | Description                                                              |
|------------------------------------------------------|------------|--------------------------------------------------------------------------|
| [tarantool3-operator](./charts/tarantool3-operator)  | current    | Operator for Tarantool 3 clusters (`db.tarantool.io/v2alpha1`).          |
| [tarantool-operator](./charts/tarantool-operator)    | legacy     | Operator for Tarantool 2 / Cartridge clusters (`tarantool.io`).          |
| [cartridge](./charts/cartridge)                      | legacy     | Tarantool Cartridge (Tarantool 2) application template.                  |

The legacy charts are kept for existing Cartridge installations and are marked
`deprecated` in their `Chart.yaml`. New deployments should use the
`tarantool3-operator` chart; Tarantool 3 clusters are then described directly
with `Cluster` and `ReplicaSet` resources (see
[`config/samples/tarantool3`](../config/samples/tarantool3)) — no application
chart is required.

Do not install `tarantool3-operator` and the legacy `tarantool-operator` in the
same Kubernetes cluster: the Tarantool 3 operator binary also contains the
legacy controllers, so two deployments would fight over the same `tarantool.io`
resources.
