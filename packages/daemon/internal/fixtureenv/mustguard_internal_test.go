package fixtureenv

import "errors"

// childGuardFailure is the error the child process of TestMustGuardEndsATestProcessWhoseGuardFails
// hands MustGuard's failure handling, in place of one from Guard.
var childGuardFailure = errors.New("fixtureenv: unset GIT_DIR: the child's own failure")

// RunGuardFailureAndExit is what the test binary runs when it is re-entered as that child: the
// half of MustGuard that runs when Guard fails, which prints and ends the process. Guard itself
// returns no error where these tests run (guardFailed's doc says why), so the child drives that
// half with an error of its own.
func RunGuardFailureAndExit() {
	guardFailed(childGuardFailure)
}

// ChildGuardFailureLine is what the child's stderr holds, the line MustGuard prints for a Guard
// that failed. It is derived from the error the child hands it: restated, a drift between the two
// would fail the assertion for the wrong reason.
var ChildGuardFailureLine = "TestMain: " + childGuardFailure.Error() + "\n"
