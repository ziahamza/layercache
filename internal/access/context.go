package access

import "context"

type claimsContextKey struct{}

func WithClaims(ctx context.Context, claims Claims) context.Context {
	ctx = context.WithValue(ctx, claimsContextKey{}, claims)
	if claims.RunID != "" {
		ctx = WithRunID(ctx, claims.RunID)
	}
	return ctx
}

func ClaimsFromContext(ctx context.Context) (Claims, bool) {
	claims, ok := ctx.Value(claimsContextKey{}).(Claims)
	return claims, ok
}
