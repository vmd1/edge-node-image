//go:build debugauth

package main

import "github.com/vmd1/edge-node-image/internal/auth"

const debugBuild = true

func debugAuthorizer() auth.Authorizer { return auth.AllowAll{} }
