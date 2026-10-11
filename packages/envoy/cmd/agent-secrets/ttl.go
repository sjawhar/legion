// packages/envoy/cmd/agent-secrets/ttl.go
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
)

// ttlFlag is --ttl's usage line, the request form's and the exec form's alike.
const ttlFlag = "how long the grant lasts, a whole number of seconds such as 2m or 90s; unset, it lasts until this session ends"

// ttlSeconds is --ttl's value as the whole seconds a request object signs: nil when flags never
// set it, and an error for a value that is not a positive whole number of seconds or is past the
// most the broker records (record.ValidLifetime), so `--ttl 0` is refused rather than read as
// unset.
func ttlSeconds(flags *flag.FlagSet, ttl time.Duration) (*int, error) {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == "ttl" })
	if !set {
		return nil, nil
	}
	if ttl%time.Second != 0 || !record.ValidLifetime(int(min(ttl/time.Second, math.MaxInt32+1))) {
		return nil, fmt.Errorf("--ttl %s is not a whole number of seconds from 1s to %ds", ttl, math.MaxInt32)
	}
	seconds := int(ttl / time.Second)
	return &seconds, nil
}

// grantedLifetime checks that the broker gave result's grant the lifetime --ttl asked for. A
// broker or agent-secrets-helper from before --ttl drops the lifetime without refusing the
// request, and the grant would then last longer than asked. What this invocation made is ended
// before it can release a secret: the grant it was given, or the request it left pending. What an
// earlier call made is never ended: an approved grant the broker reused (it carries a record), and
// a pending request this call joined (`coalesced`, which a broker from before --ttl answers for
// any request naming the same secrets, whatever lifetime either asked for); the error names it.
// This check stays: a client can meet an older broker or helper on every future independent
// rollout.
func grantedLifetime(ctx context.Context, c *client, signer Signer, result RequestResult, ttl *int) error {
	if ttl == nil || result.State == "denied" || (result.LifetimeSeconds != nil && *result.LifetimeSeconds == *ttl) {
		return nil
	}
	given := "no lifetime"
	if result.LifetimeSeconds != nil {
		given = fmt.Sprintf("a lifetime of %ds", *result.LifetimeSeconds)
	}
	mismatch := fmt.Sprintf("the broker gave request %s %s, not --ttl %ds", result.RequestID, given, *ttl)
	const why = "the broker or this machine's agent-secrets-helper is from before --ttl; restart the helper on the pinned release, or request without --ttl"
	switch {
	case result.GrantID != nil && result.RecordID != nil:
		return fmt.Errorf("%s, so nothing was run and the existing grant %s was left as approved: %s", mismatch, *result.GrantID, why)
	case result.State == "pending" && result.Coalesced:
		return fmt.Errorf("%s, so nothing was run and the existing pending request %s was left waiting for its approver: %s", mismatch, result.RequestID, why)
	case result.GrantID != nil:
		if err := c.RevokeGrant(ctx, signer, *result.GrantID); err != nil {
			return fmt.Errorf("%s, and revoking its grant %s failed: %w", mismatch, *result.GrantID, err)
		}
		return fmt.Errorf("%s, so its grant %s was ended and nothing was run: %s", mismatch, *result.GrantID, why)
	case result.State == "pending":
		if err := c.CancelRequest(ctx, signer, result.RequestID); err != nil {
			return fmt.Errorf("%s, and cancelling it failed: %w", mismatch, err)
		}
		return fmt.Errorf("%s, so the request was ended and nothing was run: %s", mismatch, why)
	}
	return fmt.Errorf("%s, so nothing was run: %s", mismatch, why)
}
