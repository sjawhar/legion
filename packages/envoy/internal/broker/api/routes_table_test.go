// This file is package api rather than api_test, the package almost every test file in this
// directory is in, for one reason: it reads the broker's own route table (routes()) and the power
// each accountAuth row declares, neither of which is worth exporting so a test can see it.
package api

import "testing"

// TestEveryAccountRouteDeclaresItsPower walks the broker's route table and fails on a route a
// caller's ServiceAccount token authenticates that declares no power. A row that declared none
// would be refused every caller at runtime (Account.may), which is the safe half; this is the
// other half, so that a route added to the accountAuth class is a deliberate choice of authority
// — Dispatch's relay of a person's decisions, or reading a token back — rather than a row that
// inherits whichever one the row above it happened to have, or none.
func TestEveryAccountRouteDeclaresItsPower(t *testing.T) {
	accountRoutes := 0
	for _, route := range routes() {
		if route.Handler.auth != authAccount {
			continue
		}
		accountRoutes++
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			if route.Handler.power == noPower {
				t.Fatalf("%s %s is an accountAuth route declaring no power: give the row the authority it needs (accountAuth(readsTokensBack, …) or accountAuth(relaysDecisions, …)), since a caller listed for one power must not reach it by being listed at all",
					route.Method, route.Pattern)
			}
			if callerHolding(route.Handler.power) == "" {
				t.Fatalf("%s %s declares a power no caller name in accountPowers holds, so no caller can ever reach it", route.Method, route.Pattern)
			}
		})
	}
	// A table that lost its accountAuth rows, or an auth constant renamed out from under this
	// walk, would otherwise pass this test by having nothing to check.
	if accountRoutes == 0 {
		t.Fatal("the route table has no accountAuth route at all: this test walked nothing")
	}
}

// callerHolding is the name of some caller that holds p, or "" when no caller name does.
func callerHolding(p power) string {
	for name, powers := range accountPowers {
		if (Account{Name: name, powers: powers}).may(p) {
			return name
		}
	}
	return ""
}
