//go:build debugauth

package auth

import "context"

// AllowAll exists only in binaries built with -tags debugauth (the debug image profile and tests). Release builds
// do not contain it, so no environment variable or flag can turn authorization off in production.
type AllowAll struct{}

func (AllowAll) Authorize(context.Context) error { return nil }
