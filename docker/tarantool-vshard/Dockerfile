# Tarantool 3 + vshard — the image a sharded Cluster needs.
#
# The community tarantool/tarantool:3 image does not bundle the vshard module:
# any replica set with sharding.roles makes Tarantool require('vshard') and the
# instance exits with "The vshard-ee/vshard module is not available". This image
# adds the pure-Lua vshard rock on top of the official image, installed into
# /usr/local/share/tarantool (already on Tarantool's package.path), so no
# LUA_PATH wiring is needed — user-provided LUA_PATH env in pod templates keeps
# working untouched.
#
# Build:
#   docker build -t tarantool-vshard:3 docker/tarantool-vshard
# Pin versions:
#   docker build --build-arg TARANTOOL_TAG=3.7.0 --build-arg VSHARD_VERSION=0.1.41 \
#     -t tarantool-vshard:3.7.0 docker/tarantool-vshard

ARG TARANTOOL_TAG=3
FROM tarantool/tarantool:${TARANTOOL_TAG}

# vshard release rock (pure Lua). Release rocks ship as .all.rock zip archives,
# so unzip is needed at install time only; the scm-1 dev rockspec would need git
# instead — pin a release. https://github.com/tarantool/vshard/releases
ARG VSHARD_VERSION=0.1.41

RUN set -eux; \
    apt-get update -qq; \
    apt-get install -y -qq --no-install-recommends unzip; \
    mkdir -p /tmp/rocks /usr/local/share/tarantool; \
    cd /tmp/rocks; \
    tt rocks install vshard "${VSHARD_VERSION}"; \
    cp -r .rocks/share/tarantool/vshard /usr/local/share/tarantool/; \
    cd /; \
    rm -rf /tmp/rocks; \
    apt-get purge -y -qq unzip; \
    apt-get autoremove -y -qq; \
    rm -rf /var/lib/apt/lists/*; \
    tarantool -e 'require("vshard") print("vshard OK") os.exit()'

# Entrypoint, user and ports are inherited from the official image.
