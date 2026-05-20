package storage

import (
	"context"
	"strings"
	"time"
)

type timezoneContextKey struct{}

func WithTimezone(ctx context.Context, timezone string) context.Context {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" || timezone == "local" {
		return ctx
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return ctx
	}
	return context.WithValue(ctx, timezoneContextKey{}, timezone)
}

func timezoneLocationFromContext(ctx context.Context) *time.Location {
	if ctx != nil {
		if timezone, ok := ctx.Value(timezoneContextKey{}).(string); ok && timezone != "" {
			if loc, err := time.LoadLocation(timezone); err == nil {
				return loc
			}
		}
	}
	return time.Local
}
