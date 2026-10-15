package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) { // `go install …@latest` has no ldflags
	info := func(v string, ok bool) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Main: debug.Module{Version: v}}, ok }
	}
	for _, tc := range []struct {
		v    string
		info func() (*debug.BuildInfo, bool)
		want string
	}{
		{"v1.2.3", info("v9", true), "v1.2.3"},
		{"dev", info("v0.1.0", true), "v0.1.0"},
		{"dev", info("(devel)", true), "dev"},
		{"dev", info("", false), "dev"},
	} {
		if got := buildVersion(tc.v, tc.info); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.v, got, tc.want)
		}
	}
}
