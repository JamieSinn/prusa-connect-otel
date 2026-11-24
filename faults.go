package main

import (
	"encoding/json"
	"net/http"

	"github.com/prusa3d/prusa-connect/sdk/fault"
)

// faultDomain is the module's namespace, derived by the host from the module
// path. Error codes are registered under it so clients can match on them.
var faultDomain = mustDomain("ca.jamiesinn.prusa-connect-otel")

var routeNotFound = fault.Register(faultDomain, "ROUTE_NOT_FOUND", http.StatusNotFound, fault.Document(
	"This module does not serve that path",
	"See openapi.json for the module's routes. Paths are relative to the module's mount.",
	fault.RetryNever,
	fault.ShowDetail,
	"path",
))

func mustDomain(name string) fault.Domain {
	d, err := fault.NewDomain(name)
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
