# Tarantool 2 + Cartridge application image for the coexistence e2e: a minimal
# "kv" cartridge app (role app.roles.kv) the legacy operator can drive. Cartridge
# is EOL-era and not in any stock image, so it is built from the rock here.
#
# Build: docker build -t cartridge-kv:2 docker/cartridge-kv
FROM tarantool/tarantool:2.11

# The base is Alpine; Cartridge's C parts (membership, ddl, http) need a
# toolchain at install time only (apk, not apt).
RUN apk add --no-cache build-base cmake git unzip \
 && mkdir -p /app \
 && cd /app \
 && tarantoolctl rocks install cartridge 2.8.6

# Make the app's local rock tree importable regardless of cwd.
ENV LUA_PATH="/app/.rocks/share/tarantool/?.lua;/app/.rocks/share/tarantool/?/init.lua;/app/?.lua;/app/?/init.lua;;" \
    LUA_CPATH="/app/.rocks/lib/tarantool/?.so;;" \
    TARANTOOL_APP_NAME="kv" \
    TARANTOOL_WORKDIR="/var/lib/tarantool" \
    TARANTOOL_RUN_DIR="/var/run/tarantool" \
    CARTRIDGE_RUN_DIR="/var/run/tarantool"

COPY app/ /app/
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh \
 && mkdir -p /var/lib/tarantool \
 # fail the build if cartridge or the app role can't load
 && tarantool -e 'require("cartridge"); require("roles.kv"); print("cartridge-kv OK"); os.exit()'

ENTRYPOINT ["/entrypoint.sh"]
