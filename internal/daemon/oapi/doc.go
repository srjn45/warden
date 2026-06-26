// Package oapi holds the code generated from warden's OpenAPI document
// (internal/daemon/apidocs/openapi.yaml), which is the single source of truth
// for the daemon's HTTP API. The generated StrictServerInterface is implemented
// by *daemon.Server; regenerate with `go generate ./...` after editing the spec.
package oapi

//go:generate go tool oapi-codegen -config config.yaml ../apidocs/openapi.yaml
