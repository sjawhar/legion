package policy

import "time"

// ListLag is listLag, for the external tests that step a Current's clock past it.
const ListLag = listLag

// SetNow makes now the clock c dates its rereads by and expires them against.
func SetNow(c *Current, now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}
