package bus

// WithReconnectWaitTick runs tick at each pass of the client's wait for a reconnect
// (ensureConnWithContext), before the pass blocks, and reads a connection event tick causes before
// the wait looks at its context, so a test outside the package can tell a publish is inside that
// wait, or act at the moment the wait ends. It is compiled into the package's tests only; no
// production connect can set it.
func WithReconnectWaitTick(tick func()) ConnectOption {
	return clientOption(func(o *connectOpts) { o.reconnectWaitTick = tick })
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
