package appauth

import (
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// AppRoleFor is the one role-to-App decision. Keep this switch exhaustive over claim.Role, plus
// the one non-claim role it also decides: the project controller, which acts as the review App
// (it writes no issue branch). Roles that can write an issue branch use implement; every
// verdict-only role, and the controller, uses review.
func AppRoleFor(role claim.Role) AppRole {
	switch role {
	case claim.RoleArchitect, claim.RolePlanner, claim.RoleTester, claim.RoleReviewer, claim.RoleController:
		return Review
	case claim.RoleImplementer, claim.RoleMerger:
		return Implement
	}
	panic(fmt.Sprintf("AppRoleFor: unsupported claim role %q", role))
}

// SessionRole is the claim role an operator-launched session pod's gh bundle is minted as
// (internal/api's session routes, and the Sandbox runtime's refresh of its Secret): a session
// writes to the owner it names, so it is the implement App's bundle, the one a claim's
// implementer pod already holds for its issue's owner. The route that mints it and the refresher
// that mints it again read this one name, so the two can never ask for different Apps.
const SessionRole = claim.RoleImplementer
