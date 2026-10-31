package bus

// WithReconnectWaitTick runs tick after each tick of the client's wait for a reconnect
// (ensureConnWithContext), before the connection is read again, so a test outside the package can
// tell a publish is inside that wait, or act at the moment the wait ends. It is compiled into the
// package's tests only; no production connect can set it.
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
