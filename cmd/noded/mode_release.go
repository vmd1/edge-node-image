//go:build !debugauth

package main

import "github.com/vmd1/edge-node-image/internal/auth"

const debugBuild = false

func debugAuthorizer() auth.Authorizer { return nil }
