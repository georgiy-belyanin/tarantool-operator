# Interacting with the local sharded Tarantool 3 cluster

`test.sh` leaves you with a kind cluster running a sharded Tarantool 3 demo:
one router (`router-a`, group `routers`) and two 2-instance storage replica
sets (`storage-a`, `storage-b`, group `storages`), manual failover, vshard
buckets bootstrapped. Pod name = instance name; `<rs>-0` is each set's leader.

All `tt` examples run *inside* a pod (the image ships `tt`); nothing needs to
be installed locally except `kubectl`.

## Operator resources (kubectl)

```sh
# The custom resources (short names: ttc = Cluster, ttrs = ReplicaSet).
kubectl get ttc
kubectl get ttrs
kubectl get ttrs -o wide

# Status: phase, ready instances, observed leader, conditions.
kubectl get ttrs storage-a -o jsonpath='{.status}' | jq
kubectl describe ttrs storage-a          # includes events
kubectl get ttc demo -o jsonpath='{.status.phase}{"\n"}'

# Everything the operator created for the cluster.
kubectl get pods,sts,svc,pdb,secrets -l tarantool.io/cluster=demo
kubectl get pods -o wide                 # instance placement

# The rendered Tarantool 3 config delivered to every instance.
kubectl get secret demo-config -o jsonpath='{.data.config\.yaml}' | base64 -d

# Operator events (reloads, schema upgrades, remediation, warnings).
kubectl get events --sort-by=.lastTimestamp | tail -20
```

## Day-2 operations (kubectl)

```sh
# Scale a storage replica set (scale subresource; new instance joins and syncs).
kubectl scale ttrs storage-a --replicas=3
kubectl rollout status statefulset/storage-a

# Hot-reload a dynamic option — log.level is applied via config:reload() over
# iproto, NO pod restart (watch: the pods' RESTARTS stay 0).
kubectl patch ttc demo --type merge -p '{"spec":{"config":{"log":{"level":6}}}}'

# A non-dynamic change (e.g. a new env var / image) rolls the pods one by one.
kubectl patch ttrs storage-a --type merge \
  -p '{"spec":{"podTemplate":{"spec":{"containers":[{"name":"tarantool","image":"tarantool-vshard:3"}]}}}}'

# Move the manual leader of a replica set (replicaset-scope config key).
kubectl patch ttrs storage-a --type merge -p '{"spec":{"config":{"leader":"storage-a-1"}}}'

# Rotate a password: edit the Secret; the operator re-renders and hot-reloads.
kubectl patch secret demo-creds --type merge -p '{"stringData":{"admin":"new-secret"}}'

# Instance logs.
kubectl logs storage-a-0 -f
```

## Talking to Tarantool with tt

Every pod has a **local admin console socket** (no auth — the same one the
readiness probe uses), and **iproto on :3301** (authenticated; users from
`config.credentials`).

```sh
# Interactive console on an instance (exit with Ctrl-D).
kubectl exec -it storage-a-0 -- tt connect /var/run/tarantool/admin.socket

# One-shot eval over the admin socket.
kubectl exec storage-a-0 -- sh -c \
  "echo 'return box.info.status' | tt connect /var/run/tarantool/admin.socket"
kubectl exec storage-a-0 -- sh -c \
  "echo 'return box.info.ro, box.info.name' | tt connect /var/run/tarantool/admin.socket"

# Replication health of a set (id, upstream status per peer).
kubectl exec storage-a-0 -- sh -c \
  "echo 'return box.info.replication' | tt connect /var/run/tarantool/admin.socket"

# Which config version is the instance running? (the operator's reload label)
kubectl exec storage-a-0 -- sh -c \
  "echo 'return require(\"config\"):get(\"labels\")' | tt connect /var/run/tarantool/admin.socket"

# Authenticated iproto session as the super user, through the headless Service
# DNS (any pod can reach any instance by name).
kubectl exec -it router-a-0 -- \
  tt connect admin:admin-secret@storage-a-0.demo.default.svc.cluster.local:3301
```

## vshard

```sh
# Router view: known replica sets, bucket distribution, alerts.
kubectl exec router-a-0 -- sh -c \
  "echo 'return vshard.router.info()' | tt connect /var/run/tarantool/admin.socket"

# Storage view on a leader.
kubectl exec storage-a-0 -- sh -c \
  "echo 'return vshard.storage.info().bucket' | tt connect /var/run/tarantool/admin.socket"

# Write/read THROUGH the router (bucket-routed):
kubectl exec router-a-0 -- sh -c "echo '
  local r = vshard.router
  local bucket = r.bucket_id_strcrc32(\"hello\")
  return r.callrw(bucket, \"box.schema.space.create\", {\"kv\", {if_not_exists = true}})
' | tt connect /var/run/tarantool/admin.socket"
```

(For real data-plane code, create spaces with a `bucket_id` field + index on
every storage and route with `vshard.router.callrw/callro` — see the vshard
docs; the demo above just proves routing works.)

## From your machine (port-forward)

```sh
# Forward the router's iproto port and connect with a *local* tt (if installed).
kubectl port-forward pod/router-a-0 3301:3301 &
tt connect admin:admin-secret@127.0.0.1:3301
```

## Failure drills

```sh
# Kill a storage replica: the StatefulSet recreates it; it rejoins from the leader.
kubectl delete pod storage-a-1
kubectl get pods -w

# Watch the operator react (leader observation, reload retries, remediation).
tail -f /tmp/tarantool-local-operator.log
```
