// Package buildinfo carries the version stamped into the binary at link time.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Values injected with -ldflags "-X github.com/curruwilla/vaultd/internal/buildinfo.Version=…".
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

func init() {
	if Version != "" && Commit != "" {
		return
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if Version == "" {
		Version = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if Commit == "" {
				Commit = setting.Value
			}
		case "vcs.time":
			if Date == "" {
				Date = setting.Value
			}
		}
	}
}

// Short returns the version alone, for the User-Agent and manifests.
func Short() string {
	if Version == "" {
		return "dev"
	}
	return Version
}

// String returns the full one-line build description.
func String() string {
	commit := Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	out := "vaultd " + Short()
	if commit != "" {
		out += " (" + commit + ")"
	}
	if Date != "" {
		out += " built " + Date
	}
	return fmt.Sprintf("%s, %s %s/%s", out, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Info is the build description as data, for the metrics build_info gauge and
// the server's /api/version.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns the build metadata of the running binary.
func Get() Info {
	return Info{
		Version:   Short(),
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}
