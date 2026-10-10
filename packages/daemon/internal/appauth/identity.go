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
