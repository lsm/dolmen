package version

import "runtime/debug"

var Version = ""

func init() {
	if Version == "" {
		info, _ := debug.ReadBuildInfo()
		Version = fromBuildInfo(info)
	}
}

func fromBuildInfo(info *debug.BuildInfo) string {
	if info == nil {
		return "devel"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "devel"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		return "devel+" + rev + "-dirty"
	}
	return "devel+" + rev
}
