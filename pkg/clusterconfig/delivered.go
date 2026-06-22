package clusterconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"sigs.k8s.io/yaml"
)

// Delivered is a parsed rendered-config payload (the config Secret's content).
// Parse once with ParseDelivered, then ask it everything the operator needs to
// know about the delivered config — instead of re-unmarshalling the same bytes
// for every question.
type Delivered struct {
	tree map[string]any
}

// ParseDelivered parses the rendered config delivered to instances.
func ParseDelivered(renderedConfig []byte) (*Delivered, error) {
	var tree map[string]any
	if err := yaml.Unmarshal(renderedConfig, &tree); err != nil {
		return nil, fmt.Errorf("clusterconfig: parsing rendered config: %w", err)
	}
	return &Delivered{tree: tree}, nil
}

// Failover modes of replication.failover, as the operator's gates read them
// from the delivered config. FailoverOff is Tarantool's default when unset.
const (
	FailoverOff        = "off"
	FailoverManual     = "manual"
	FailoverElection   = "election"
	FailoverSupervised = "supervised"
)

// DefaultIprotoPort is the operator's conventional iproto port, used when the
// user config sets no iproto.listen. (3301 is a Tarantool convention, not a
// Tarantool default — unset listen means no iproto at all.)
const DefaultIprotoPort int32 = 3301

// Settings are the few effective configuration values the operator's own
// behavior depends on, read back from the rendered config. The rendered config
// is the single merged truth — regardless of whether a value came from a typed
// spec field or a raw config section — so reading it here keeps the controllers
// working unchanged as typed fields migrate into config sections.
//
// Defaults mirror Tarantool's own (an unset key means Tarantool's default).
type Settings struct {
	// Failover is the effective replication.failover mode ("off" when unset —
	// Tarantool's default).
	Failover string
	// Leader is the replicaset-scope leader key ("" when unset).
	Leader string
	// Port is the iproto port of the replica set's instances (3301 when not
	// derivable — Tarantool's conventional default).
	Port int32
	// SuperUser is the first global-scope credentials user carrying the "super"
	// role ("" when none) — the user the operator authenticates as for iproto
	// operations (leader observation, config reload, schema upgrade).
	SuperUser string
	// OffModeRW is, in failover "off", the instance whose own (instance-scope)
	// config sets database.mode: rw — the writable instance the user designated.
	// "" when no instance-scope rw exists (the operator's instance-0 default
	// applies, or every instance is rw via a broader scope). Off mode has no
	// failover, so status.leader and remediation protection must follow the
	// actually-writable instance, not assume ordinal 0.
	OffModeRW string
}

// ContainsReplicaSet reports whether the config includes the subtree for
// groups.<group>.replicasets.<replicaSet>. The Cluster reconciler can briefly
// render a config before its child ReplicaSets are observed in cache (informer
// warmup), producing a topology-less config; a ReplicaSet must not build its
// StatefulSet from such a config, or the StatefulSet's config-hash would change
// once the real subtree appears and roll the pods (which, on ephemeral storage,
// re-bootstraps the designated bootstrap leader into its own split replica set).
func (d *Delivered) ContainsReplicaSet(group, replicaSet string) bool {
	_, ok := lookup(d.tree, "groups", group, "replicasets", replicaSet).(map[string]any)
	return ok
}

// chain builds the delivered config's scope chain for one replica set: a first
// instance (where the operator generates listen), then the replica-set, group
// and global scopes.
func (d *Delivered) chain(group, replicaSet string) scopeChain {
	rsTree, _ := lookup(d.tree, "groups", group, "replicasets", replicaSet).(map[string]any)
	sc := scopeChain{}
	if instances, ok := lookup(rsTree, "instances").(map[string]any); ok {
		for _, inst := range instances {
			sc = append(sc, inst)
			break
		}
	}
	return append(sc, rsTree, lookup(d.tree, "groups", group), d.tree)
}

// Settings reads the effective Settings for one replica set, applying
// Tarantool's scope precedence for the keys settable at several scopes.
func (d *Delivered) Settings(group, replicaSet string) Settings {
	rsTree, _ := lookup(d.tree, "groups", group, "replicasets", replicaSet).(map[string]any)
	sc := d.chain(group, replicaSet)

	s := Settings{
		Failover: sc.str(FailoverOff, "replication", "failover"),
		Port:     sc.listenPort(DefaultIprotoPort),
		// The operator's iproto user: its own built-in user when delivered with
		// the super role, else the first (name-ordered) super user.
		SuperUser: deliveredSuperUser(d.tree),
	}
	// leader: a replicaset-scope key.
	if v, ok := lookup(rsTree, "leader").(string); ok {
		s.Leader = v
	}
	if s.Failover == FailoverOff {
		s.OffModeRW = offModeWritableInstance(rsTree)
	}
	return s
}

// deliveredSuperUser picks the user the operator authenticates as: its own
// built-in OperatorUser when the delivered config carries it with the super
// role (the default), otherwise the first (name-ordered) super-role user the
// cluster credentials declare.
func deliveredSuperUser(tree map[string]any) string {
	if userHasRole(tree, OperatorUser, "super") {
		return OperatorUser
	}
	return firstUserWithRole(tree, "super")
}

// offModeWritableInstance returns the (name-ordered) first instance whose own
// scope sets database.mode: rw, "" when none does.
func offModeWritableInstance(rsTree map[string]any) string {
	instances, _ := lookup(rsTree, "instances").(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(instances)) {
		if mode, _ := lookup(instances[name], "database", "mode").(string); mode == "rw" {
			return name
		}
	}
	return ""
}

// InstanceCount returns how many instances the delivered config declares for
// the replica set. The scale-up gate compares it with spec.replicas: new pods
// may only be created once the delivered topology already lists them (and the
// existing instances have loaded it) — otherwise the newcomer joins a master
// whose loaded config does not contain it, and the master's autoexpel expels
// it on the spot.
func (d *Delivered) InstanceCount(group, replicaSet string) int {
	instances, _ := lookup(d.tree, "groups", group, "replicasets", replicaSet, "instances").(map[string]any)
	return len(instances)
}

// IsShardStorage reports whether a replica set's effective config makes it a
// vshard storage (sharding.roles contains "storage"). The operator uses it to
// decide whether a replica set needs drain-gated deletion.
func (d *Delivered) IsShardStorage(group, replicaSet string) bool {
	return d.hasShardingRole(group, replicaSet, "storage")
}

// IsShardRouter reports whether a replica set's effective config makes it a
// vshard router (sharding.roles contains "router"). The operator bootstraps
// vshard through a router once the storages are reachable.
func (d *Delivered) IsShardRouter(group, replicaSet string) bool {
	return d.hasShardingRole(group, replicaSet, "router")
}

func (d *Delivered) hasShardingRole(group, replicaSet, role string) bool {
	roles, _ := d.chain(group, replicaSet).value("sharding", "roles").([]any)
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// dynamicScopePaths are config paths (within the {global, replicaset} scope map)
// that Tarantool applies at runtime via config:reload() without a restart. The
// rollout hash excludes them so a change to only these does not roll the pods (the
// operator reloads them instead). Listed conservatively — only keys that are
// well-established box.cfg dynamic options — because mis-listing a NON-dynamic key
// here would let it change without a restart and silently not take effect, whereas
// omitting a dynamic key merely causes an unnecessary (but correct) rollout.
var dynamicScopePaths = [][]string{
	{"global", "log", "level"},           // box.cfg.log_level
	{"global", "memtx", "memory"},        // box.cfg.memtx_memory (grow-only)
	{"global", "replication", "timeout"}, // box.cfg.replication_timeout
	{"global", "credentials"},            // users/roles/passwords reload at runtime
	{"replicaset", "sharding", "weight"}, // vshard storage weight
	// config.roles / config.roles_cfg: Tarantool applies (and stops) roles on
	// config:reload() — the core of the Tarantool 3 role mechanism — so adding,
	// removing, or reconfiguring a role does not need a restart. Stripped at both
	// the global and replica-set scope (the typed API sets roles at either level).
	// A newly enabled role whose Lua module is not yet require-able makes the
	// reload FAIL loudly (surfaced + retried), not silently no-op — so this is
	// safe under the "mis-listing silently drops a change" caveat above.
	{"global", "roles"},
	{"global", "roles_cfg"},
	{"replicaset", "roles"},
	{"replicaset", "roles_cfg"},
}

// alwaysStripPaths are excluded from every scope hash: the operator-internal config
// version label (which changes on every config change by construction) and
// replication.autoexpel (toggled on post-bootstrap; only governs future scale-down,
// which restarts pods anyway).
var alwaysStripPaths = [][]string{
	{"global", "labels", ConfigHashLabel},
	{"replicaset", "replication", "autoexpel"},
}

// InstanceScopeHash returns a stable hash of the configuration that affects one
// replica set's instances: the global (cluster-wide) scope plus that replica
// set's own subtree, excluding every other replica set (so a change to a different
// replica set does not change this hash). The operator-internal
// config-version label and autoexpel are excluded.
//
// Re-marshalled as canonical (key-sorted) JSON so the hash is stable despite
// go-config's non-deterministic YAML key order.
func (d *Delivered) InstanceScopeHash(group, replicaSet string) (string, error) {
	return d.scopeHash(group, replicaSet, false)
}

// RolloutHash is like InstanceScopeHash but also excludes what config:reload()
// applies without a restart: the dynamic keys (dynamicScopePaths) and instance
// MEMBERSHIP. Per instance the operator-generated wiring (iproto.listen/advertise,
// derived from the instance name) is stripped and instances left empty are
// dropped, so scaling adds/removes pods without rolling the survivors — they learn
// the new peer list on the next reload. An instance's own config CONTENT
// (spec.instanceConfigs) stays in the hash: instance-scope statics cannot be
// reload-applied, so changing them must roll the pods. (InstanceScopeHash keeps
// `instances` whole, so without a super user a topology change still rolls.)
func (d *Delivered) RolloutHash(group, replicaSet string) (string, error) {
	return d.scopeHash(group, replicaSet, true)
}

func (d *Delivered) scopeHash(group, replicaSet string, stripDynamic bool) (string, error) {
	tree := d.tree

	// Global scope: every top-level key except the per-replica-set topology.
	global := map[string]any{}
	for k, v := range tree {
		if k == "groups" {
			continue
		}
		global[k] = v
	}

	// This replica set's own subtree under groups.<group>.replicasets.<name>.
	rsTree := lookup(tree, "groups", group, "replicasets", replicaSet)

	// Deep-copy so deleting paths never mutates the parsed tree.
	scope, _ := deepCopyJSON(map[string]any{"global": global, "replicaset": rsTree}).(map[string]any)
	for _, p := range alwaysStripPaths {
		deletePath(scope, p)
	}
	if stripDynamic {
		for _, p := range dynamicScopePaths {
			deletePath(scope, p)
		}
		// Instance membership is reload-applied (a joining/leaving peer is a
		// config:reload, not a restart), so adding/removing instances must not
		// change the rollout hash and roll the surviving pods — but an instance's
		// own config content must (instance-scope statics need a restart).
		stripGeneratedInstanceWiring(scope)
	}

	canonical, err := json.Marshal(scope)
	if err != nil {
		return "", fmt.Errorf("clusterconfig: hashing instance scope: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// stripGeneratedInstanceWiring reduces the scope's replicaset.instances map to
// the user-borne config content: the operator-generated per-instance wiring
// (iproto.listen and iproto.advertise — deterministic functions of the instance
// name, delivered via config:reload) is removed, and instances left with no other
// content are dropped entirely. The rollout hash thus ignores pure membership
// changes (scaling) but still tracks each instance's own config content.
func stripGeneratedInstanceWiring(scope map[string]any) {
	instances, ok := lookup(scope, "replicaset", "instances").(map[string]any)
	if !ok {
		return
	}
	for name, inst := range instances {
		m, ok := inst.(map[string]any)
		if !ok {
			continue
		}
		// deletePath prunes an iproto map left empty by the removals.
		deletePath(m, []string{"iproto", "listen"})
		deletePath(m, []string{"iproto", "advertise"})
		if len(m) == 0 {
			delete(instances, name)
		}
	}
}

// MemtxMemory extracts the global memtx.memory value, with ok=false when unset.
// Used to detect a DECREASE between the delivered and newly rendered config:
// memtx.memory is grow-only at runtime, so a decrease cannot be hot-reloaded and
// needs a restart — the operator warns instead of going silently stale.
func (d *Delivered) MemtxMemory() (int64, bool) {
	// ParseDelivered decodes via sigs.k8s.io/yaml (YAML -> JSON -> encoding/json),
	// so every number in the tree is a float64 — no other numeric shape can occur.
	if v, ok := lookup(d.tree, "memtx", "memory").(float64); ok {
		return int64(v), true
	}
	return 0, false
}

// lookup walks nested string-keyed maps; returns nil when any step is missing.
func lookup(v any, path ...string) any {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

// portOf extracts the numeric port from a "host:port" URI.
func portOf(uri string) (int32, bool) {
	for i := len(uri) - 1; i >= 0; i-- {
		if uri[i] == ':' {
			var p int32
			if _, err := fmt.Sscanf(uri[i+1:], "%d", &p); err == nil && p > 0 {
				return p, true
			}
			return 0, false
		}
	}
	return 0, false
}

// deletePath removes a nested key (path) from m if present, pruning ancestor
// maps the removal left empty. The pruning matters for hashing: an empty map
// must hash identically to an absent one, or the FIRST appearance of a section
// that only carries stripped keys (say, adding log.level to a config with no
// log section at all) would change the rollout hash and roll the pods that the
// stripping exists to protect.
func deletePath(m map[string]any, path []string) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		delete(m, path[0])
		return
	}
	next, ok := m[path[0]].(map[string]any)
	if !ok {
		return
	}
	deletePath(next, path[1:])
	if len(next) == 0 {
		delete(m, path[0])
	}
}

// deepCopyJSON deep-copies a JSON-like value (maps, slices, scalars).
func deepCopyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		c := make(map[string]any, len(t))
		for k, e := range t {
			c[k] = deepCopyJSON(e)
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, e := range t {
			c[i] = deepCopyJSON(e)
		}
		return c
	default:
		return v
	}
}
