package clusterconfig

import (
	"maps"
	"slices"
)

// scopeChain is an ordered list of config scope maps, most specific first
// (instance > replica set > group > global) — Tarantool's effective-value
// precedence. It is the ONE precedence reader, shared by the pre-render
// userCfg (walking the CR's raw config sections) and the post-render
// Delivered (walking the delivered YAML), so the two can never drift apart.
type scopeChain []any

// value returns the setting at path from the most specific scope that has it,
// or nil when unset everywhere.
func (sc scopeChain) value(path ...string) any {
	for _, scope := range sc {
		if v := lookup(scope, path...); v != nil {
			return v
		}
	}
	return nil
}

// str returns the string setting at path, or def when unset.
func (sc scopeChain) str(def string, path ...string) string {
	if v, ok := sc.value(path...).(string); ok {
		return v
	}
	return def
}

// listenPort extracts the port of the first iproto.listen URI in the chain,
// defaulting to def when no scope sets a listen or the URI has no numeric port.
func (sc scopeChain) listenPort(def int32) int32 {
	listen, _ := sc.value("iproto", "listen").([]any)
	if len(listen) == 0 {
		return def
	}
	if uri, ok := lookup(listen[0], "uri").(string); ok {
		if p, ok := portOf(uri); ok {
			return p
		}
	}
	return def
}

// hasListen reports whether any scope in the chain sets iproto.listen.
func (sc scopeChain) hasListen() bool {
	v, _ := sc.value("iproto", "listen").([]any)
	return len(v) > 0
}

// firstUserWithRole returns the first (in name order) credentials user in
// globalScope carrying role ("" when none). Credentials are read at the global
// scope only — both the renderer (advertise logins) and the introspector (super
// user) key off it. Name order matters: with several matching users a map-order
// pick would make the operator's identity flap between reconciles.
func firstUserWithRole(globalScope map[string]any, role string) string {
	users, _ := lookup(globalScope, "credentials", "users").(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(users)) {
		if userHasRole(globalScope, name, role) {
			return name
		}
	}
	return ""
}

// userHasRole reports whether the named global-scope credentials user carries
// role.
func userHasRole(globalScope map[string]any, name, role string) bool {
	roles, _ := lookup(globalScope, "credentials", "users", name, "roles").([]any)
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}
