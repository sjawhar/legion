# Classification fixtures

The Go classifier replay (`packages/daemon-go/internal/classify/fixtures_test.go`) replays every record here byte-exactly. A record is one JSON object with `fn`, `input` and `output`, in the directory named for its `fn`.

Records change only by hand. A record's bytes are its canonical JSON (keys sorted, no whitespace, no trailing newline), which `jq -cjS . <record>` prints. Its file name is the SHA-256 of its input's canonical JSON, which `jq -cjS .input <record> | sha256sum` prints. `TestFixtureRecordsFollowTheDirectoryContract` fails on a record that is in another function's directory or breaks either rule.
