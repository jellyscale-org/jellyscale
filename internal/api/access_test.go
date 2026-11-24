package api

import (
	"log/slog"
	"testing"
)

// JS-125. Every route declares who may call it (Route.Access: "anyone" or "viewer"), none is open by default, and an
// open one says why (Route.Why). docs/access.md is the model and a row per route. TestAccess
// (access_integration_test.go) asks every route over HTTP.

func routeName(r Route) string { return r.Method + " " + r.Path }

// openList is the routes anyone may call, which TestAccess reads as the expected answers. The pictures are open by the
// owner's decision of 2026-11-16 (jellyfin-web builds picture URLs with no token, an <img> cannot send one, and item ids
// are random).
var openList = []string{
	"GET /System/Info/Public",
	"GET /System/Ping",
	"POST /System/Ping",
	"GET /Branding/Configuration",
	"GET /Branding/Css",
	"GET /Branding/Css.css",
	"GET /QuickConnect/Enabled",
	"GET /Users/Public",
	"POST /Users/AuthenticateByName",
	"GET /Items/{itemId}/Images/{imageType}",
	"GET /Items/{itemId}/Images/{imageType}/{imageIndex}",
}

// Server.Handler() refuses a route whose Access is not declared or is not "anyone" or "viewer", whichever roles this
// server has: a binary must not start with a route that nobody decided. The real tree has no such route, so this holds
// that the build is quiet for each set of roles.
func TestHandlerStartsOnlyWithEveryAccessDeclared(t *testing.T) {
	for _, roles := range [][]string{{RoleAPI}, {RoleStream}, {RoleAPI, RoleStream}} {
		recovered := func() (r any) {
			defer func() { r = recover() }()
			(&Server{Roles: roles, Log: slog.New(slog.DiscardHandler)}).Handler()
			return nil
		}()
		if recovered != nil {
			t.Errorf("Handler with roles %v panicked: %v", roles, recovered)
		}
	}
}
