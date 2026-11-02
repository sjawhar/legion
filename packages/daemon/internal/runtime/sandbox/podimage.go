package sandbox

// IssuePodRunsConfiguredImage says whether the pod of issue's Sandbox is running the image the
// daemon's configuration names (r.image), read from the pod the runtime's store holds: the pod
// must be the Sandbox's own, not ended, and no container of it — a role's or an init container —
// may run another image. That last reading is imageDrift's, the one a relaunch replaces a pod on
// (relaunch.go), so a release and a relaunch never disagree about which pods are behind.
//
// An idle release reads this before it releases a pod, because a released pod comes back as a
// fresh pod on the configured image: releasing a pod that already runs another image would wake
// its roles onto a daemon release they never ran under, and a pod on an image this daemon does
// not serve is a pod a relaunch of any of its roles replaces outright. Anything that leaves the
// image unread — no Sandbox, no pod, a pod of another Sandbox, an ended pod — answers false: a
// pod whose image cannot be read is not on the configured image, so the release holds rather
// than guesses.
func (r *Runtime) IssuePodRunsConfiguredImage(issue string) bool {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return false
	}
	s, err := r.storedSandbox(name)
	if err != nil || s == nil {
		return false
	}
	pod := r.storedPod(name)
	if !ownedBy(pod, s.UID) || terminal(pod) {
		return false
	}
	_, drifted := imageDrift(pod, r.image)
	return !drifted
}
