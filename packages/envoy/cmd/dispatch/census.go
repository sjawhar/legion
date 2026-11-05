package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/mentiongraph"
	"github.com/sjawhar/envoy/internal/dispatch/persongraph"
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

// runCensus preserves the pre-deploy migration census with no arguments. --deliveries is a
// post-deploy read only: it never migrates the database it is checking.
func runCensus(ctx context.Context, args []string, databaseURL string, out, errOut io.Writer) int {
	if len(args) == 0 {
		return census(ctx, databaseURL, out, errOut)
	}
	if len(args) != 1 || (args[0] != "--deliveries" && args[0] != "--mentions") {
		fmt.Fprintln(errOut, "usage: envoy-dispatch census [--deliveries|--mentions]")
		return 2
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "census: DATABASE_URL is required")
		return 2
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintln(errOut, "census:", err)
		return 2
	}
	defer database.Pool.Close()
	if args[0] == "--mentions" {
		report, err := mentiongraph.Census(ctx, database.Pool, time.Now().Add(-30*24*time.Hour))
		if err != nil {
			fmt.Fprintln(errOut, "census:", err)
			return 2
		}
		if err := json.NewEncoder(out).Encode(report); err != nil {
			fmt.Fprintln(errOut, "census:", err)
			return 2
		}
		if report.Checked == 0 || report.Recorded == 0 {
			fmt.Fprintln(errOut, "census: no recorded identity mentions in the last 30 days")
			return 1
		}
		if report.Mismatched != 0 {
			return 1
		}
		return 0
	}
	report, err := persongraph.Census(ctx, database.Pool, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		fmt.Fprintln(errOut, "census:", err)
		return 2
	}
	if err := json.NewEncoder(out).Encode(report); err != nil {
		fmt.Fprintln(errOut, "census:", err)
		return 2
	}
	if report.Checked == 0 {
		fmt.Fprintln(errOut, "census: no comments, messages or asks in the last 30 days")
		return 1
	}
	if report.Mismatched != 0 {
		return 1
	}
	return 0
}
