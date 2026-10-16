package policy

import "time"

// ListLag is the AWS store's ListLag, which every Local-backed test runs on, for the external tests
// that step a Current's clock past it.
const ListLag = 5 * time.Minute

// SetNow makes now the clock c dates its rereads by and expires them against.
func SetNow(c *Current, now func() time.Time) {
	c.mu <- struct{}{}
	defer c.unlock()
	c.now = now
}
