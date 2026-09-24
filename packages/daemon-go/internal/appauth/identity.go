package appauth

import (
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// AppRoleFor is the one role-to-App decision. Keep this switch exhaustive over claim.Role: roles
// that can write an issue branch use implement; every verdict-only role uses review.
func AppRoleFor(role claim.Role) AppRole {
	switch role {
	case claim.RoleArchitect, claim.RolePlanner, claim.RoleTester, claim.RoleReviewer:
		return Review
	case claim.RoleImplementer, claim.RoleMerger:
		return Implement
	}
	panic(fmt.Sprintf("AppRoleFor: unsupported claim role %q", role))
}
