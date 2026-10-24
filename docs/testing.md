# Testing

Rules for this repository's tests that the code under test does not show.

## Unix sockets in Go tests

A unix socket path must be 107 bytes or shorter. Linux's `sun_path` holds 108 bytes with the
terminating zero, and Go refuses a longer path with `invalid argument` at `bind`, `listen` or
`dial`. A tmux server socket has the same limit (`Filename too long`). `t.TempDir()` puts the
test's name, cut to 64 bytes, in its path, under `GOTMPDIR` or else `TMPDIR`. With a long test name
and a socket under it, or under a directory made from it (an `XDG_RUNTIME_DIR`, `HOME` or state
directory a test sets to `t.TempDir()`), a temporary directory of about 20 bytes is enough to pass
the limit, as on a dev box whose `TMPDIR` is on a data disk.

- Put a test's socket under a short directory of its own: `os.MkdirTemp("", "<short prefix>")`,
  removed in `t.Cleanup`. A package that binds sockets in its tests keeps one helper for it,
  `shortTempDir` (`packages/daemon/internal/daemon/daemon_test.go` is one). Use the package's
  helper, or add one where the package has none.
- Before you call a test that binds a socket done, run it with a temporary directory of about 64
  bytes. A pass under `/tmp` does not show that the test works under a longer one. Set `GOTMPDIR`
  as well, since `t.TempDir()` reads it first:

  ```sh
  L=/tmp/a-deliberately-long-tmpdir-for-unix-socket-path-tests-0001 && mkdir -p "$L"
  TMPDIR="$L" GOTMPDIR="$L" go test ./<package>/... -count=1
  ```
