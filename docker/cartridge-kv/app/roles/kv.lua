-- Shared kv business logic, wrapped as a Cartridge cluster role.
-- The same space + functions are used by the Tarantool 3 cluster
-- (test/e2e/testdata/coexist/kv-app-t3.lua) — only the wrapper differs.
local function make_schema()
    local kv = box.schema.space.create('kv', {
        if_not_exists = true,
        format = {{name = 'id', type = 'string'}, {name = 'value', type = 'string'}},
    })
    kv:create_index('pk', {parts = {'id'}, if_not_exists = true})
end

local function init(opts)
    if opts.is_master then
        make_schema()
    end
    -- public API (identical to the T3 app)
    rawset(_G, 'kv_put', function(id, value) box.space.kv:replace({id, value}) return true end)
    rawset(_G, 'kv_get', function(id) local t = box.space.kv:get(id); return t and t.value or nil end)
end

return {
    role_name = 'app.roles.kv',
    init = init,
    validate_config = function() return true end,
    apply_config = function() end,
    dependencies = {},
}
