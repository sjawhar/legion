package launcher

// The layout of an issue pod's roles directory (--roles-dir), which the pod builder
// (internal/runtime/sandbox) mounts and `legion launcher` and its role-init read: one directory per
// role, holding the role's launcher token projection (RoleLauncherDir, its token file
// LauncherTokenFile), its gh bundle (RoleGHDir), and its own memory-backed private, state and
// agent-secrets directories (RolePrivateDir, RoleStateDir, RoleAgentSecretsDir); and RoleHomesDir,
// the one disk directory, which holds each role's home overlay layers and its own temporary
// directories. Both sides name these constants, so a renamed directory moves on both.
const (
	RoleLauncherDir     = "launcher"
	RoleGHDir           = "gh"
	RolePrivateDir      = "private"
	RoleStateDir        = "state"
	RoleAgentSecretsDir = "agent-secrets"
	RoleHomesDir        = ".homes"
	LauncherTokenFile   = "LAUNCHER_TOKEN"
)
