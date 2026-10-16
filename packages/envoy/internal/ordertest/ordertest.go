// Package ordertest calls a test with every order of a list, for a test that shows a result does
// not depend on the order its input arrives in: GitHub's listing order, and the order a store read
// answers rows tied on its order in, are neither of them the order a result may rest on. It is an
// ordinary package rather than a _test.go file because Go cannot share test-only code across
// packages (see internal/stacktest's identical reasoning).
package ordertest

import "slices"

// EveryOrder calls f with a copy of items in each of its orders, len(items)! calls in all.
func EveryOrder[T any](items []T, f func([]T)) {
	var permute func(k int)
	permute = func(k int) {
		if k == len(items) {
			f(slices.Clone(items))
			return
		}
		for i := k; i < len(items); i++ {
			items[k], items[i] = items[i], items[k]
			permute(k + 1)
			items[k], items[i] = items[i], items[k]
		}
	}
	permute(0)
}
