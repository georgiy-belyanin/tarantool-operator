#!/usr/bin/env tarantool
-- Cartridge application entrypoint. The legacy operator always bootstraps
-- vshard, so the app registers the built-in vshard roles alongside the shared
-- kv role; the storage replicaset carries [vshard-storage, app.roles.kv] and a
-- router replicaset carries [vshard-router] (assigned via each Role's
-- clusterRoles). advertise_uri / http_port / workdir / cluster_cookie come from
-- TARANTOOL_* env (set by the entrypoint from the pod FQDN).
require('cartridge').cfg({
    roles = {
        'cartridge.roles.vshard-router',
        'cartridge.roles.vshard-storage',
        'roles.kv',  -- module /app/roles/kv.lua; role_name app.roles.kv
    },
    vshard_groups = {default = {bucket_count = 300}},
})
