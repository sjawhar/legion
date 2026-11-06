package bus

// WithReconnectWaitTick runs tick at each pass of the client's wait for a reconnect
// (ensureConnWithContext), before the pass blocks, and reads a connection event tick causes before
// the wait looks at its context, so a test outside the package can tell a publish is inside that
// wait, or act at the moment the wait ends. It is compiled into the package's tests only; no
// production connect can set it.
func WithReconnectWaitTick(tick func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.reconnectWaitTick = tick })
}

// ReconnectWorkDone reports whether the client has worked through every connection event its
// callbacks handed it (queueReconnect): nothing is queued and no goroutine of the client's is
// working one. The work of a reconnect no longer runs on nats.go's callback goroutine, so a test
// that reads what that work decided waits for this first. It is compiled into the package's
// tests only.
func (c *Client) ReconnectWorkDone() bool {
	c.reconnectQueueMu.Lock()
	defer c.reconnectQueueMu.Unlock()
	return !c.reconnectRunning && len(c.reconnectQueue) == 0
}

// WithBeforeRequestFlush runs before on the goroutine that flushes a Request's publish, right
// before that flush begins, so a test outside the package can have the connection lost by the time
// the flush starts - the interleaving in which nats.go answers the flush with nothing, having
// cleared its pending flushes as the connection turned reconnecting. The wait for the request's
// answer runs beside that goroutine and is not held by the hook. It is compiled into the package's
// tests only; no production connect can set it.
func WithBeforeRequestFlush(before func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.beforeRequestFlush = before })
}
