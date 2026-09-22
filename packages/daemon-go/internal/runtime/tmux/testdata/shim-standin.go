// Command shim-standin stands in for `legion worker-shim` in the tmux runtime's real-tmux tests,
// so the runtime's pane command runs end to end before the real shim (Task 2.3) and the real
// listener (Task 2.4) meet in one process tree (Task 2.10's integration test).
//
// It takes exactly the argv the runtime builds — `worker-shim --connect unix://<path>
// --boot-token-file <file> -- <omp argv…>` — dials the address, sends the hello with the file's
// token, starts OMP only after the hello_ack, and bridges lines both ways. A shutdown frame is
// answered by SIGTERM to OMP, never forwarded. It exits with OMP's exit code.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim-standin:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) < 6 || args[0] != "worker-shim" || args[1] != "--connect" || args[3] != "--boot-token-file" || args[5] != "--" {
		return 0, fmt.Errorf("unexpected argv %q", args)
	}
	address, tokenFile, omp := args[2], args[4], args[6:]
	if !strings.HasPrefix(address, "unix://") || len(omp) == 0 {
		return 0, fmt.Errorf("unexpected argv %q", args)
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return 0, err
	}
	conn, err := net.Dial("unix", strings.TrimPrefix(address, "unix://"))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	fromDaemon, toDaemon := shimwire.NewReader(conn), shimwire.NewWriter(conn)
	if err := toDaemon.WriteFrame(shimwire.Hello{BootToken: strings.TrimSpace(string(token))}); err != nil {
		return 0, err
	}
	line, err := fromDaemon.ReadLine()
	if err != nil {
		return 0, fmt.Errorf("await hello_ack: %w", err)
	}
	if frame, err := shimwire.Decode(line); err != nil || frame.FrameType() != shimwire.TypeHelloAck {
		return 0, errors.New("the daemon did not acknowledge the hello")
	}

	child := exec.Command(omp[0], omp[1:]...)
	child.Stderr = os.Stderr
	stdin, err := child.StdinPipe()
	if err != nil {
		return 0, err
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := child.Start(); err != nil {
		return 0, err
	}
	go func() {
		fromOmp := shimwire.NewReader(stdout)
		for {
			line, err := fromOmp.ReadLine()
			if err != nil {
				return
			}
			if toDaemon.WriteLine(line) != nil {
				return
			}
		}
	}()
	go func() {
		toOmp := shimwire.NewWriter(stdin)
		for {
			line, err := fromDaemon.ReadLine()
			if err != nil {
				return
			}
			if frame, err := shimwire.Decode(line); err == nil && frame.FrameType() == shimwire.TypeShutdown {
				_ = child.Process.Signal(syscall.SIGTERM)
				continue
			}
			if toOmp.WriteLine(line) != nil {
				return
			}
		}
	}()
	err = child.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}
