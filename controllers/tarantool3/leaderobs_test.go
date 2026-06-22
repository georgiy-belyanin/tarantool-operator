package tarantool3

import (
	"testing"
)

// TestNeedsNetworkLeaderObservation is the RS2 decision logic: only election and
// supervised modes read the leader over the network (and thus need a super user);
// manual and off declare the writable instance, so no network read is required.
func TestNeedsNetworkLeaderObservation(t *testing.T) {
	cases := map[string]bool{
		"manual":     false,
		"off":        false,
		"election":   true,
		"supervised": true,
	}
	for mode, want := range cases {
		if got := needsNetworkLeaderObservation(mode); got != want {
			t.Errorf("needsNetworkLeaderObservation(%q) = %v, want %v", mode, got, want)
		}
	}
	// Default (unset) failover is "off" → no network observation.
	if got := needsNetworkLeaderObservation("off"); got {
		t.Errorf("needsNetworkLeaderObservation(off) = true, want false")
	}
}
