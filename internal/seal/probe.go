package seal

import "context"

// WithProbe attaches a per-operation test/fault-injection seam to ctx. Existing
// sealStep boundaries invoke it synchronously. Without a probe, operations are
// unchanged. The caller must not mutate the probe while an operation uses it.
func WithProbe(ctx context.Context, probe func(step string, n int64) error) context.Context {
	if probe == nil {
		return ctx
	}
	return context.WithValue(ctx, hookKey{}, probe)
}
