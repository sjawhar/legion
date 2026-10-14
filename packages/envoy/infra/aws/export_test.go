package aws

import "github.com/sjawhar/envoy/infra/core"

// The renderings' tests are in package aws_test, so they build from the same example inputs as
// the component's.

// Core is the authorization as core.Authorization takes it.
func (a *AuthorizationArgs) Core() *core.Authorization { return a.core() }
