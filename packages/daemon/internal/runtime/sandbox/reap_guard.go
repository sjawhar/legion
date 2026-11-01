package sandbox

import (
	"context"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// guardSessionVolumeReap refuses to delete a Sandbox-owned volume when its pod
// template does not name SQL session storage and a matching claim records a
// session. The daemon deliberately has no session-database credentials, so the
// template is the discriminator rather than a lookup of the session table.
func (r *Runtime) guardSessionVolumeReap(ctx context.Context, s *sandbox) error {
	matches, ok := r.sessionVolumeClaims(s)
	if !ok || sandboxStoresSessionsInDatabase(s) {
		return nil
	}
	claims, err := r.store.Claims(ctx)
	if err != nil {
		return fmt.Errorf("read claims before reaping volume %s: %w", issueVolume+"-"+s.Name, err)
	}
	for _, c := range claims {
		if !matches(c) || c.Session == "" {
			continue
		}
		return fmt.Errorf("refuse to reap volume %s: claim %s records file-store session %s; run legion sessions import before retrying, or run legion claims forget-session --claim %s --session %s after the claim has no live process",
			issueVolume+"-"+s.Name, c.Token, c.Session, c.Token, c.Session)
	}
	return nil
}

// sessionVolumeClaims identifies the claims whose file sessions could be on
// s's Sandbox-owned issue volume. An issue Sandbox owns such a volume only in
// the superseded disk shape; a controller Sandbox always owns its working
// volume. Repository caches and current issue pods carry no claim session.
func (r *Runtime) sessionVolumeClaims(s *sandbox) (func(supervise.Claim) bool, bool) {
	kind, err := podKindOf(s.Labels)
	if err != nil || !ownsIssueVolume(s) {
		return nil, false
	}
	switch kind.(type) {
	case issuePod:
		issue := s.Labels[labelIssue]
		return func(c supervise.Claim) bool {
			return c.Project == r.project && c.Issue == issue
		}, true
	case controllerPod:
		token := claim.ControllerToken(r.project)
		return func(c supervise.Claim) bool { return c.Token == token }, true
	default:
		return nil, false
	}
}

// ownsIssueVolume is whether a Sandbox owns the issue claim template. The
// current issue-pod shape owns none; its pod owns an ephemeral disk instead.
func ownsIssueVolume(s *sandbox) bool {
	for _, template := range s.Spec.VolumeClaimTemplates {
		if template.Metadata.Name == issueVolume {
			return true
		}
	}
	return false
}

func sandboxStoresSessionsInDatabase(s *sandbox) bool {
	return sessionStoreInPod(&s.Spec.PodTemplate.Spec) != sessionStoreName(ompsessions.StoreVolume, "", nil)
}
