<a href="http://tarantool.org">
   <img src="https://static.tarantool.io/pub/221123-0838-43389b9/tarantool/images/current-logo.svg" align="right">
</a>

# Tarantool Kubernetes Operator CE

[![Tests][gh-test-actions-badge]][gh-actions-url]
[![Lint][gh-lint-actions-badge]][gh-actions-url]

This is a [Kubernetes Operator](https://coreos.com/operators/) for Tarantool clusters on Kubernetes.

The repository hosts **two** operators side by side:

* **Tarantool 3** (`db.tarantool.io/v2alpha1`) — the current operator, built on
  Tarantool 3's native [declarative cluster configuration](https://www.tarantool.io/en/doc/latest/concepts/configuration/)
  (no Cartridge). It renders the cluster config from `Cluster` + `ReplicaSet`
  custom resources and delivers it to the instances. **See [ABOUT.md](./ABOUT.md)**
  for how it works and a feature/status matrix. Its code lives in the non-`cartridge`
  packages (`apis/v2alpha1`, `controllers/tarantool3`, `pkg/clusterconfig`).
* **Tarantool 2 + Cartridge** (`tarantool.io`) — the legacy operator that deploys
  [Tarantool Cartridge](https://github.com/tarantool/cartridge)-based clusters.
  It is **deprecated**, segregated under the `cartridge/` subpackages, and kept for
  backward compatibility during migration.

If you are a Tarantool Enterprise customer, or need Enterprise features such as rolling update, scaling down and may others
you can use the [Tarantool Operator Enterprise](https://www.tarantool.io/ru/kubernetesoperator).

## IMPORTANT NOTICE

Begins from v1.0.0-rc1 Tarantool Kubernetes Operator CE was completely rewrote.

API version was bumped and any backward compatibility was dropped.

There is only one approved method to migrate from version 0.0.0 to versions >=1.0.0-rc1, 
please follow [migration guide](./docs/migrate-from-0.0.x-to-1.0.0.md).

## Table of contents

* [Getting started](#getting-started)
* [Documentation](#documentation)
* [Contribute](#contribute)

## Getting started

### Tarantool 3 (`db.tarantool.io/v2alpha1`)

- Overview, architecture, and feature/status matrix: **[ABOUT.md](./ABOUT.md)**
- Sample custom resources: [`config/samples/tarantool3/`](./config/samples/tarantool3)
  (minimal, replicated, sharded, application roles, Lua app, metrics)
- Try it on [kind](https://kind.sigs.k8s.io/): `make test-e2e` (and `-scaling`,
  `-config`, `-large`, `-luaapp`, `-roles`, `-stack`, `-leader`); see
  [`test/e2e/`](./test/e2e)

### Tarantool 2 + Cartridge (legacy)

- [Install the Operator](./docs/installation.md)
- [Deploy example application](./docs/deploy-example-application.md)

## Documentation

The documentation is work in progress...

At the moment you can use official [helm-chart](https://github.com/tarantool/helm-charts/tree/master/charts/tarantool-operator) 
and receive useful information from comments in default [values.yaml](https://github.com/tarantool/helm-charts/blob/master/charts/tarantool-operator/values.yaml) file 

## Contribute

Please follow the [development guide](./docs/development-guide.md)

[gh-lint-actions-badge]: https://github.com/tarantool/tarantool-operator/actions/workflows/lint.yml/badge.svg
[gh-test-actions-badge]: https://github.com/tarantool/tarantool-operator/actions/workflows/test.yml/badge.svg
[gh-actions-url]: https://github.com/tarantool/tarantool-operator/actions
