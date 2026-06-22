package clusterconfig

import (
	"fmt"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// userCfg is the user's raw config sections, parsed once per render. The
// operator consults them only for the handful of values its own wiring depends
// on (failover mode, leader, iproto port, database.mode presence); everything
// else is passed through for Tarantool to interpret.
type userCfg struct {
	global map[string]any
	groups map[string]map[string]any
	rs     map[string]any
	insts  map[string]map[string]any // by ordinal string
}

// parseUserCfg decodes the cluster's and one replica set's raw config sections.
func parseUserCfg(c *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet) (userCfg, error) {
	u := userCfg{groups: map[string]map[string]any{}, insts: map[string]map[string]any{}}
	var err error
	if u.global, err = jsonToMap(c.Spec.Config); err != nil {
		return u, fmt.Errorf("clusterconfig: cluster spec.config: %w", err)
	}
	for name := range c.Spec.GroupConfigs {
		raw := c.Spec.GroupConfigs[name]
		if u.groups[name], err = jsonToMap(&raw); err != nil {
			return u, fmt.Errorf("clusterconfig: group %q config: %w", name, err)
		}
	}
	if rs != nil {
		if u.rs, err = jsonToMap(rs.Spec.Config); err != nil {
			return u, fmt.Errorf("clusterconfig: replicaset %q spec.config: %w", rs.Name, err)
		}
		for ord := range rs.Spec.InstanceConfigs {
			raw := rs.Spec.InstanceConfigs[ord]
			if u.insts[ord], err = jsonToMap(&raw); err != nil {
				return u, fmt.Errorf("clusterconfig: replicaset %q instance %s config: %w", rs.Name, ord, err)
			}
		}
	}
	return u, nil
}

// chain builds the scope chain for one instance (instance > replica set >
// group > global); ordinal "" skips the instance scope.
func (u userCfg) chain(group, ordinal string) scopeChain {
	sc := scopeChain{}
	if ordinal != "" {
		sc = append(sc, u.insts[ordinal])
	}
	return append(sc, u.rs, u.groups[group], u.global)
}

// str returns the user's string setting for path, or def when unset.
func (u userCfg) str(group, ordinal, def string, path ...string) string {
	return u.chain(group, ordinal).str(def, path...)
}

// listenPort extracts the port of the user's first iproto.listen URI for the
// given instance (any scope), defaulting to def when the user set no listen or
// the URI carries no numeric port.
func (u userCfg) listenPort(group, ordinal string, def int32) int32 {
	return u.chain(group, ordinal).listenPort(def)
}

// hasListen reports whether the user configured iproto.listen at any scope for
// the given instance — in which case the operator must not generate one
// (Tarantool's instance-scope precedence would shadow the user's value, and
// arrays replace rather than merge).
func (u userCfg) hasListen(group, ordinal string) bool {
	return u.chain(group, ordinal).hasListen()
}

// ClusterPort returns the iproto port the cluster's Service should expose: the
// port of the user's global-scope iproto.listen when set, else 3301 (the
// operator's generated default).
func ClusterPort(c *v2alpha1.Cluster) int32 {
	u, err := parseUserCfg(c, nil)
	if err != nil {
		return DefaultIprotoPort
	}
	return u.listenPort("", "", DefaultIprotoPort)
}

// userWithRole returns the first global-scope credentials user carrying role.
func (u userCfg) userWithRole(role string) string {
	return firstUserWithRole(u.global, role)
}

// hasAnyDatabaseMode reports whether the user set database.mode anywhere that
// affects this replica set: at a non-instance scope or on any instance. The
// off-mode writable-instance gap-fill must stand down in either case — adding a
// generated rw instance next to a user-designated one would split writes.
func (u userCfg) hasAnyDatabaseMode(group string) bool {
	if u.chain(group, "").value("database", "mode") != nil {
		return true
	}
	for _, inst := range u.insts {
		if lookup(inst, "database", "mode") != nil {
			return true
		}
	}
	return false
}
