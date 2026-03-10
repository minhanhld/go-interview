package auth

import "context"

type contextKey string

const userIDKey contextKey = "userID"

// SetUserID stores the user ID in the context. Called by the middleware.
func SetUserID(ctx context.Context, userID string) context.Context {
    return context.WithValue(ctx, userIDKey, userID)
}

// GetUserID retrieves the user ID from the context. Called by resolvers.
func GetUserID(ctx context.Context) (string, bool) {
    val, ok := ctx.Value(userIDKey).(string)
    return val, ok && val != ""
}
