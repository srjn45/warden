package cli

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Build metadata. version defaults to "dev" for source builds and is overridden
// at release time via the linker (goreleaser sets all three from the git tag):
//
//	-ldflags "-X github.com/srjn45/warden/internal/cli.version=<tag> \
//	          -X github.com/srjn45/warden/internal/cli.commit=<sha> \
//	          -X github.com/srjn45/warden/internal/cli.date=<rfc3339>"
//
// For source builds without those flags, commit/date fall back to the VCS
// stamps Go embeds in the build info (see currentBuildInfo).
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// buildInfo is the resolved build metadata reported by `warden version` and
// `warden --version`.
type buildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go"`
	Platform  string `json:"platform"`
}

// currentBuildInfo resolves the build metadata, preferring ldflags values and
// falling back to the VCS stamps Go embeds (for `go install` / `go build`
// without explicit ldflags). Unknown fields are reported as "unknown".
func currentBuildInfo() buildInfo {
	bi := buildInfo{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi.Commit == "" || bi.Date == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				switch s.Key {
				case "vcs.revision":
					if bi.Commit == "" {
						bi.Commit = s.Value
					}
				case "vcs.time":
					if bi.Date == "" {
						bi.Date = s.Value
					}
				}
			}
		}
	}
	if bi.Commit == "" {
		bi.Commit = "unknown"
	}
	if bi.Date == "" {
		bi.Date = "unknown"
	}
	return bi
}

// String renders the human-readable build info shown by `warden --version` and
// the default `warden version` output.
func (b buildInfo) String() string {
	return fmt.Sprintf("warden %s\nCommit: %s\nBuilt: %s\nGo: %s  Platform: %s",
		b.Version, b.Commit, b.Date, b.GoVersion, b.Platform)
}

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print warden version and build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			bi := currentBuildInfo()
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(bi)
			}
			fmt.Fprintln(out, bi.String())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output build info as JSON")
	return cmd
}
