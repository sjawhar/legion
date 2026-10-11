package main

import "path/filepath"

// handoffFile is name in issue's own handoff directory, .legion/<issue>/<name>, relative to the
// workspace: the one place that layout is spelled, for every handoff a worker commits and for the
// workspace recovery marker. Each tree writes only under its own directory, so two trees running
// at once never touch the same path (dispatch://LEGION-565), and retro's last commit removes it
// from the head a human merges, so none reaches main (dispatch://LEGION-605). issue is a key
// workspace-init's --issue check accepted.
func handoffFile(issue, name string) string {
	return filepath.Join(".legion", issue, name)
}
