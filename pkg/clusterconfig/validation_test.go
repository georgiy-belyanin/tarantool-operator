package clusterconfig_test

import (
	"testing"
)

// TestRenderDoesNotValidate: the operator does not validate the configuration —
// Tarantool itself is the authority (it validates on start and on
// config:reload(), and the schema differs across versions). A passthrough value
// the operator cannot judge must render verbatim instead of being rejected by an
// operator-pinned schema copy.
func TestRenderDoesNotValidate(t *testing.T) {
	in := baseInput()
	// memtx.memory as a string would violate the 3.x schema; the operator must
	// still render it and leave the verdict to Tarantool.
	in.Cluster.Spec.Config = jsonExt(`{"memtx":{"memory":"a-lot"}}`)

	tree := renderTree(t, in)
	if got := submap(t, tree, "memtx")["memory"]; got != "a-lot" {
		t.Errorf("passthrough value must render verbatim, got %v", got)
	}
}
