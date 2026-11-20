package runner

import _ "embed"

//go:embed warm_workspace_lfs_clean.sh
var warmLFSVerificationClean string

// warmLFSVerificationArgs overrides the filter for a single baseline check.
// The persistent checkout keeps its standard Git LFS clean and smudge filters.
func warmLFSVerificationArgs(args ...string) []string {
	return append([]string{"-c", "filter.lfs.required=true", "-c", "filter.lfs.clean=" + warmLFSVerificationClean}, args...)
}
