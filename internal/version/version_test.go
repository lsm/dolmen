package version

import (
	"runtime/debug"
	"testing"
)

func TestAnUnstampedBuildNamesItsSourceNotAnOldRelease(t *testing.T) {
	cases := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"go install at a tag", &debug.BuildInfo{Main: debug.Module{Version: "v0.6.0"}}, "v0.6.0"},
		{"checkout build", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef0123"}}}, "devel+0123456789ab"},
		{"dirty checkout", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef0123"}, {Key: "vcs.modified", Value: "true"}}}, "devel+0123456789ab-dirty"},
		{"no build info", nil, "devel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fromBuildInfo(tc.info); got != tc.want {
				t.Fatalf("fromBuildInfo = %q, want %q", got, tc.want)
			}
		})
	}
}
