package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/launcher"
)

const launcherUsage = "legion launcher --connect tcp://host:port --token-file <path> --sandbox <name> --role <role> --pod-uid <uid> --boot-token-file <path>"

// runLauncher is PID 1 in one role container of an issue pod. It has no workflow policy: it only
// authenticates to the daemon and starts or stops its own worker-shim child on daemon commands.
func runLauncher(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("launcher", stderr)
	connect := flags.String("connect", "", "the daemon launcher stream: tcp://host:port")
	tokenFile := flags.String("token-file", "", "the role-private launcher token file")
	sandbox := flags.String("sandbox", "", "the issue Sandbox name")
	role := flags.String("role", "", "the role container this launcher owns")
	podUID := flags.String("pod-uid", "", "the current Kubernetes pod UID")
	bootTokenFile := flags.String("boot-token-file", "", "the role-private worker shim boot token file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(flags.Args()) != 0 {
		fmt.Fprintln(stderr, launcherUsage)
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "launcher: token file %s: %v\n", *tokenFile, err)
		return 1
	}
	if err := launcher.Run(ctx, launcher.Config{
		Connect: *connect, Token: strings.TrimSpace(string(token)), Sandbox: *sandbox, Role: *role,
		PodUID: *podUID, BootTokenFile: *bootTokenFile, Stdout: stdout, Stderr: stderr,
	}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
