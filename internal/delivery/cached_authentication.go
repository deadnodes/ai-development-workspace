package delivery

import "context"

type cachedAuthenticationKey struct{}

// WithCachedAuthentication removes authentication-refresh authority from reads
// used to prepare or reconcile an already bounded execution intent.
func WithCachedAuthentication(ctx context.Context) context.Context {
	return context.WithValue(ctx, cachedAuthenticationKey{}, true)
}

func CachedAuthenticationOnly(ctx context.Context) bool {
	value, _ := ctx.Value(cachedAuthenticationKey{}).(bool)
	return value
}
