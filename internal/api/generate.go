// Package api is the HTTP adapter of /api: the code generated from
// api/openapi.yaml and the handlers that map it to the stream and adbreak
// services.
package api

//go:generate go tool oapi-codegen -config ../../api/oapi-codegen.yaml ../../api/openapi.yaml
