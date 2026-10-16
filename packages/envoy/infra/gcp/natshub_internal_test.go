package gcp

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNameAddedUsersLeavesIndexesInQuotedRulesUntouched(t *testing.T) {
	err := errors.New(`users[1] "example-hub": subscribe.allow[0] "users[1] q" names a queue group`)

	got := nameAddedUsers(err, 1, true, false)

	require.EqualError(t, got, `listener "example-hub": subscribe.allow[0] "users[1] q" names a queue group`)
}
