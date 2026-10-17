package policy

import "time"

// SetNow makes now the clock c dates its rereads by and expires them against.
func SetNow(c *Current, now func() time.Time) {
	c.mu <- struct{}{}
	defer c.unlock()
	c.now = now
}
