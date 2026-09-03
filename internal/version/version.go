// Package version holds dolmen's single release identity.
//
// Release builds inject the version at link time, without editing source:
//
//	go build -ldflags "-X github.com/lsm/dolmen/internal/version.Version=v0.2.0"
//
// Builds without injection (go build, go run, go test) report the
// development default below; `make build` derives devel-<commit> from git.
package version

// Version is reported identically by --version, the startup log,
// GET /version, and MCP initialize serverInfo.
var Version = "0.1.0-devel"
