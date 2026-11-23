package api

import (
	"strings"
	"testing"
)

// JS-123: PATCH /jellyscale/v1/users/configuration. These checks need nothing outside Go; the behaviour is held by
// cmd/jellyscale/users_configuration_patch_integration_test.go.

const patchConfigurationRoute = "PATCH /jellyscale/v1/users/configuration"

func TestThePatchOfTheUsersConfigurationIsAViewerRouteWithNoUserId(t *testing.T) {
	var found *Route
	for _, route := range (&Server{}).Routes() {
		if routeName(route) == patchConfigurationRoute {
			found = &route
		}
	}
	switch {
	case found == nil:
		t.Fatalf("no route %s", patchConfigurationRoute)
	case found.Status != Implemented || found.Access != AccessViewer || found.Why != "":
		t.Errorf("%s: status %q, access %q, why %q; want implemented, viewer and no why", patchConfigurationRoute, found.Status, found.Access, found.Why)
	case strings.Contains(found.Path, "{"):
		t.Errorf("%s takes a parameter: the route acts on the caller and takes no user id", patchConfigurationRoute)
	}
}
