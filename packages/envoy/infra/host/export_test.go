package host

// ResolveListener is resolveListener's refusal, for host_test's refusal table, which needs no
// engine.
func ResolveListener(name string, args *ListenerArgs) error {
	_, err := resolveListener(name, args)
	return err
}
