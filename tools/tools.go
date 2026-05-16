//go:build tools

// Package tools tracks build-time tool dependencies in go.sum.
// Nothing in production imports this; the build tag excludes it from
// normal compilation. Install tools with `make swagger-install`.
package tools

import (
	_ "github.com/swaggo/swag/cmd/swag"
)
