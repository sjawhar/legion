package bus

// WithReconnectWaitTick runs tick after each tick of the client's wait for a reconnect
// (ensureConnWithContext), before the connection is read again, so a test outside the package can
// tell a publish is inside that wait, or act at the moment the wait ends. It is compiled into the
// package's tests only; no production connect can set it.
func WithReconnectWaitTick(tick func()) ConnectOption {
	return func(o *connectOpts) { o.reconnectWaitTick = tick }
}
