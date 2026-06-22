# Sourcing Tarantool passwords from HashiCorp Vault (optional)

By default the operator injects user passwords into the rendered config from the
in-namespace `spec.credentialsSecret` (each data key is a user name). Vault is an
**optional, compile-time module**: it adds a password source that reads a Vault
KV v2 secret and overlays it on the base Secret. When the operator is not built
with the `vault` tag, none of this code — or any Vault dependency — is in the
binary.

## Build the operator with Vault support

```sh
go build -tags vault -o bin/manager .
# or for the image:
docker build --build-arg GO_BUILD_TAGS=vault -t my/tarantool-operator:vault .
```

The stock build (no tag) has zero Vault code; verify with
`go tool nm bin/manager | grep -i vault` (empty).

## Opt a cluster in (annotations — no CRD change)

A Vault-built operator activates the source per cluster, only when all three
annotations are present on the `Cluster`:

```yaml
apiVersion: db.tarantool.io/v2alpha1
kind: Cluster
metadata:
  name: mycluster
  annotations:
    tarantool.io/vault-address: https://vault.example.svc:8200
    # KV v2 data path after /v1/ — the secret's data map is user -> password
    tarantool.io/vault-path: secret/data/tarantool/mycluster
    # in-namespace Secret holding the Vault token under key "token"
    tarantool.io/vault-token-secret: mycluster-vault-token
spec:
  config:
    credentials:
      users:
        replicator: {roles: [replication]}
        admin:      {roles: [super]}
  # credentialsSecret is optional now: anything not in Vault can still come from it.
  # credentialsSecret: mycluster-creds
```

The Vault KV v2 secret at `secret/data/tarantool/mycluster` would hold:

```json
{ "replicator": "...", "admin": "..." }
```

## Semantics

- **Overlay**: passwords are resolved from `credentialsSecret` first, then the
  Vault map is layered on top (Vault wins for a user present in both). So you can
  keep some passwords in a Secret and source others from Vault.
- **Dormant unless configured**: without the annotations the source does nothing,
  even in a Vault build.
- **Rotation**: the operator re-reads Vault on every reconcile; a rotated value
  is delivered (and hot-reloaded, with a `super` user) on the next reconcile /
  resync.
- **Token**: read from the named in-namespace Secret's `token` key. (Kubernetes
  auth / agent-injected tokens are a future addition — the read path is a small,
  dependency-free KV v2 GET, easy to extend.)
