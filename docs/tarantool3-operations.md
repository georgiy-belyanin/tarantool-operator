# Tarantool 3 operator — operations guide

Operational guidance for clusters managed by the `db.tarantool.io/v2alpha1`
operator: durability, backup, networking, and upgrades.

## Durability & persistence

Tarantool persists data as **snapshots** (`.snap`) plus a **write-ahead log**
(`.xlog`); on restart an instance recovers from the latest snapshot and replays
the trailing WAL. Both live under the instance's data directories (the official
image defaults them to `/var/lib/tarantool`).

**For production, give every instance a PersistentVolumeClaim** mounted at the
data directory:

```yaml
spec:
  podTemplate:
    spec:
      containers:
        - name: tarantool
          volumeMounts:
            - name: data
              mountPath: /var/lib/tarantool
  volumeClaimTemplates:
    - metadata: { name: data }
      spec:
        accessModes: [ReadWriteOnce]
        resources: { requests: { storage: 10Gi } }
```

What this buys you:

- **Restarts and rollouts recover locally** from snapshot+WAL instead of
  re-fetching everything from the leader.
- **PVC retention**: the operator sets the StatefulSet PVC retention policy to
  `Retain` for both deletion and scale-down — deleting a `ReplicaSet` (or scaling
  it down) never deletes data volumes; clean them up explicitly when you mean it.
- **Volume expansion**: grow a volume by editing the PVC's
  `spec.resources.requests.storage` (the StorageClass must set
  `allowVolumeExpansion: true`). Shrinking is not supported by Kubernetes.

Risk model without PVCs (`emptyDir`): any pod restart discards the replica's
data; it can rebuild from the leader, but losing the **leader's** volume with
unreplicated synchronous transactions is unrecoverable. Keep `emptyDir` for
demos only.

`memtx.memory` is **grow-only at runtime**: increases hot-reload; a decrease
cannot be applied to a running instance (the operator emits a
`MemtxShrinkRequiresRestart` warning event) — restart the pods to apply it.

## Backup & restore

The operator does not yet manage backups; use Tarantool's built-in hot-backup
primitives until it does:

1. **Consistent hot copy.** On the instance (e.g. `kubectl exec` over the admin
   socket), pin the current checkpoint files, copy them off, release:

   ```lua
   files = box.backup.start()  -- returns the exact files to copy
   -- copy <files> out of the pod, e.g. kubectl cp / object storage
   box.backup.stop()
   ```

   `box.snapshot()` forces a fresh checkpoint first if you want the copy to be
   as current as possible.
2. **Restore** by placing the copied `.snap`/`.xlog`/`.vylog` files into a fresh
   instance's data directory (empty PVC) before it starts; it recovers from them
   and rejoins replication.
3. **Volume snapshots.** With a CSI driver that supports `VolumeSnapshot`,
   snapshotting the data PVC after `box.snapshot()` gives a crash-consistent
   per-instance backup at the storage layer.

A scheduled `Backup` CRD (driving `box.backup` to object storage) is the planned
follow-up; the storage backend choice is deliberately not baked in yet.

## Network policy

Tarantool's iproto traffic is **unencrypted in Community Edition** (TLS and
`pap-sha256` auth are Enterprise features), so restrict who can reach the
instances. Flows to allow:

- **peer ↔ peer** on the iproto port (replication) within the cluster's pods;
- **operator → instances** on iproto (leader observation, config reload, schema
  upgrade) from the operator's namespace;
- **clients → instances** on iproto from your application namespaces;
- kubelet probes are host-local and unaffected by NetworkPolicy.

Example (adjust namespaces/labels):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: example-tarantool
spec:
  podSelector:
    matchLabels: { tarantool.io/cluster: example }
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector: # peers
            matchLabels: { tarantool.io/cluster: example }
        - namespaceSelector: # the operator
            matchLabels: { kubernetes.io/metadata.name: tarantool-operator-system }
        - namespaceSelector: # your clients
            matchLabels: { kubernetes.io/metadata.name: my-app }
      ports:
        - { protocol: TCP, port: 3301 }
```

## Binary upgrades

Patch the tarantool image in `spec.podTemplate`; the StatefulSet rolls instances
one at a time (readiness-gated), the leader steps down gracefully before its
restart (election failover), and once **all** instances run the new binary the
operator runs `box.schema.upgrade()` on the leader (tracked in
`status.schemaUpgradedForImage`, `SchemaUpgraded` event). Replication is
supported between adjacent minor versions, so upgrade one minor at a time. To
pin pre-upgrade behaviors of changed defaults, set the `compat` section via
passthrough `spec.config`.
