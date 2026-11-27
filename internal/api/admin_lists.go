package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
)

// The lists jellyfin-web's user pages read besides the users themselves.

// mediaFolders lists the libraries.
func (s *Server) mediaFolders(w http.ResponseWriter, r *http.Request) {
	libraries, err := s.Items.Libraries(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	folders := []map[string]any{}
	for _, library := range libraries {
		folders = append(folders, s.Items.LibraryDTO(library))
	}
	writeJSON(w, map[string]any{"Items": folders, "TotalRecordCount": len(folders), "StartIndex": 0})
}

// noCapabilities is the object the schema requires of a device: none are stored.
func noCapabilities() map[string]any {
	return map[string]any{"PlayableMediaTypes": []any{}, "SupportedCommands": []any{},
		"SupportsMediaControl": false, "SupportsPersistentIdentifier": false}
}

// listDevices lists each signed-in device once, as its latest sign-in names it. A token is never listed.
func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `SELECT DISTINCT ON (d.device_id) d.device_id, d.device_name, d.client, d.app_version,
			u.id::text, u.name, d.created_at
		FROM devices d JOIN users u ON u.id = d.user_id ORDER BY d.device_id, d.created_at DESC`)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	devices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (map[string]any, error) {
		var id, name, app, version, userID, userName string
		var signedIn time.Time
		err := row.Scan(&id, &name, &app, &version, &userID, &userName, &signedIn)
		return map[string]any{"Id": id, "Name": name, "AppName": app, "AppVersion": version,
			"LastUserId": db.CompactID(userID), "LastUserName": userName,
			"DateLastActivity": signedIn.UTC().Format(time.RFC3339Nano), "Capabilities": noCapabilities()}, err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, map[string]any{"Items": devices, "TotalRecordCount": len(devices), "StartIndex": 0})
}
