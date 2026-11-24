package main

import (
	"encoding/json"
	"net/http"

	"github.com/prusa3d/prusa-connect/sdk/extension"
	"github.com/prusa3d/prusa-connect/sdk/fault"
)

// extensionModule is the module path the extension is published under, the
// same as go.mod's. The host and the marketplace derive the extension id from
// it: com.github.aux.jamiesinn.connect-otel.
const extensionModule = "github.com/jamiesinn/connect-otel"

// faultDomain is the module's namespace. Error codes are registered under it
// so clients can match on them.
var faultDomain = mustDomain(extensionModule)

var routeNotFound = fault.Register(faultDomain, "ROUTE_NOT_FOUND", http.StatusNotFound, fault.Document(
	"This module does not serve that path",
	"See openapi.json for the module's routes. Paths are relative to the module's mount.",
	fault.RetryNever,
	fault.ShowDetail,
	"path",
))

var notConfigured = fault.Register(faultDomain, "NOT_CONFIGURED", http.StatusConflict, fault.Document(
	"The exporter is not configured",
	"Set otlp_endpoint on the Settings tab.",
	fault.RetryAllowed,
	fault.ShowDetail,
))

func mustDomain(module string) fault.Domain {
	namespace, err := extension.NamespaceOf(module)
	if err != nil {
		panic(err)
	}

	d, err := fault.NewDomain(string(namespace))
	if err != nil {
		panic(err)
	}

	return d
}

// writeFault writes f as an RFC 9457 problem response.
func writeFault(w http.ResponseWriter, r *http.Request, f *fault.Fault) {
	problem := f.ProblemJSON(r.URL.Path)

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(problem.Status)

	_ = json.NewEncoder(w).Encode(problem)
}
