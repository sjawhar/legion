//go:build !linux

package launcher

import (
	"context"
	"errors"
	"syscall"
)

// The test server's namespaces are Linux's, as a role's are: an issue pod's container runs the
// worker image, and the launcher's multi-role mode is that container's alone. Every other platform
// this binary is built for refuses them rather than serving a superuser's cluster unseparated.
var errTestPGNamespacesNeedLinux = errors.New("launcher: the test database's own namespaces need Linux")

func testPGNamespaceAttr() (*syscall.SysProcAttr, error) { return nil, errTestPGNamespacesNeedLinux }

func testPGInit(context.Context, TestPGInitConfig) error { return errTestPGNamespacesNeedLinux }
