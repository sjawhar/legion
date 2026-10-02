package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// census is `envoy-dispatch census`: the pre-deploy migration census (store.Census) of the
// database DATABASE_URL names, written to out. A deployment runs it, as a one-off task under the
// service's own database credentials, before it rolls the service to this binary's image. Exit 0
// when nothing refuses the pending migrations, 1 when the report refuses (every reason is in the
// report), 2 when the census could not be taken (errOut says why). It prints counts, sizes,
// versions, file names, session metadata and SQLSTATEs only; no row leaves the database.
func census(ctx context.Context, databaseURL string, out, errOut io.Writer) int {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "census: DATABASE_URL is required")
		return 2
	}
	report, err := store.Census(ctx, databaseURL)
	if err != nil {
		message := err.Error()
		if !strings.HasPrefix(message, "census: ") {
			message = "census: " + message
		}
		fmt.Fprintln(errOut, message)
		return 2
	}
	report.Write(out)
	if report.Refused() {
		return 1
	}
	return 0
}
