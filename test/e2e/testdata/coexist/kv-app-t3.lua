-- Tarantool 3 form of the shared kv app (same business logic as the Cartridge
-- role docker/cartridge-kv/app/roles/kv.lua): a kv space + kv_put/kv_get.
-- Loaded via config.app.file; the schema is created once the instance is writable.
box.watch('box.status', function(_, status)
    if status.is_ro == false and box.space.kv == nil then
        local kv = box.schema.space.create('kv', {
            if_not_exists = true,
            format = {{name = 'id', type = 'string'}, {name = 'value', type = 'string'}},
        })
        kv:create_index('pk', {parts = {'id'}, if_not_exists = true})
    end
end)

function _G.kv_put(id, value) box.space.kv:replace({id, value}) return true end
function _G.kv_get(id) local t = box.space.kv:get(id); return t and t.value or nil end
