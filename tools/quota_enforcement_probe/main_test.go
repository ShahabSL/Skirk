package main

import "testing"

func TestPhasePass(t *testing.T) {
	ok := func(who string) observation { return observation{Consumer: who, Status: 200, Reason: "ok_exact_id"} }
	deny := observation{Consumer: "selected", Status: 403, Reason: "RATE_LIMIT_EXCEEDED"}
	cases := []struct {
		phase string
		rows  []observation
		want  bool
	}{
		{"baseline", []observation{ok("selected"), ok("publisher")}, true},
		{"challenged", []observation{ok("publisher"), deny, deny, ok("publisher")}, true},
		{"challenged", []observation{ok("publisher"), deny, ok("selected"), ok("publisher")}, false},
		{"restored", []observation{ok("selected")}, true},
		{"restored", []observation{deny}, false},
		{"restored", []observation{{Consumer: "selected", Status: 403, Reason: "PERMISSION_DENIED"}}, false},
	}
	for _, tc := range cases {
		if got := phasePass(tc.phase, tc.rows); got != tc.want {
			t.Errorf("phase=%s rows=%v: got %t, want %t", tc.phase, tc.rows, got, tc.want)
		}
	}
}
