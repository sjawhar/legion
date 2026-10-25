//go:build !linux

package launcher

import (
	"context"
	"errors"
	"syscall"
)

// A role's namespaces are Linux's: an issue pod's container runs the worker image, and the
// launcher's multi-role mode is that container's alone. Every other platform this binary is
// built for refuses it rather than running a role unseparated.
var errRoleNamespacesNeedLinux = errors.New("launcher: a role's own namespaces need Linux")

func roleNamespaceAttr() (*syscall.SysProcAttr, error) { return nil, errRoleNamespacesNeedLinux }

func roleInit(context.Context, RoleInitConfig) error { return errRoleNamespacesNeedLinux }
