package model

import "context"

// A sync, or a forwarded read, works by the settings of its upstream as it
// read them: which namespaces are selected and how widely entries are shown.
// An administrator may change those settings while it is still working. So
// that nothing is then written by the old settings, the work carries the
// version of the settings it read in its context, and every store mutation
// made under that context checks it inside its own transaction (ordered
// against UpdateUpstream) and fails with ErrUpstreamConfigChanged when the
// settings have changed.

type upstreamGuardKey struct{}

type upstreamGuard struct{ id, version int64 }

// WithUpstreamGuard returns a context under which store mutations are only
// applied while the upstream's ConfigVersion is still configVersion.
func WithUpstreamGuard(ctx context.Context, upstreamID, configVersion int64) context.Context {
	return context.WithValue(ctx, upstreamGuardKey{}, upstreamGuard{upstreamID, configVersion})
}

// UpstreamGuardFrom returns the guard carried by ctx, if any.
func UpstreamGuardFrom(ctx context.Context) (upstreamID, configVersion int64, ok bool) {
	g, ok := ctx.Value(upstreamGuardKey{}).(upstreamGuard)
	return g.id, g.version, ok
}
