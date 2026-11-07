package store

import "embed"

// MigrationFiles is the embedded migration set, for the external test package: store_test's
// migration rows apply single files by hand to pin what each one does to the database a build
// before it left (store_test.go), and the set is unexported so that nothing outside the package
// applies a migration itself.
var MigrationFiles embed.FS = migrationFiles
