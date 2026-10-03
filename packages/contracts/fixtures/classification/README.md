# Classification fixtures

The Go classifier replay (`packages/daemon-go/internal/classify/fixtures_test.go`) replays every record here byte-exactly. Each is one canonical JSON record of a classifier input and its output, named by the SHA-256 of its canonical input.

The records came from the TypeScript reducer suite's recorder (`packages/daemon/src/daemon/__tests__/fixture-recorder.ts`). They hold nothing of that daemon's GitHub reads, which the Go daemon does not make: no record of its rollup fence decision or of a read winning a head clock's tie over a webhook, and no reconciled flag on a pull request. Re-recording would bring them back, so a record changes by hand and takes its new input's hash as its name.
