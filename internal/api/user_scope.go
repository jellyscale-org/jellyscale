package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
)

// requestUserIDs returns every user id the request names, in the path, in any
// query value and, on a route with UserIDBody, in the body. An empty value is
// no id. Reading that body leaves it whole for its handler.
func requestUserIDs(r *http.Request, route Route) ([]string, error) {
	var ids []string
	if id := r.PathValue("userId"); id != "" {
		ids = append(ids, id)
	}
	for _, key := range []string{"userid", "controllablebyuserid"} {
		for _, id := range r.URL.Query()[key] {
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	if !route.UserIDBody {
		return ids, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		// The handler judges a body that is not JSON; there is no id to guard.
		return ids, nil
	}
	for name, raw := range fields {
		if !strings.EqualFold(name, "UserId") {
			continue
		}
		var id string
		if json.Unmarshal(raw, &id) != nil {
			ids = append(ids, string(raw))
			continue
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func sameUser(left, right string) bool {
	return db.ValidID(left) && db.ValidID(right) && strings.EqualFold(db.CompactID(left), db.CompactID(right))
}

func allOwnUser(ids []string, caller string) bool {
	for _, id := range ids {
		if !sameUser(id, caller) {
			return false
		}
	}
	return true
}

// requireUserScope is the one guard every viewer route passes before its
// handler. The default is own: an administrator gets no other user's view.
func (s *Server) requireUserScope(w http.ResponseWriter, r *http.Request, route Route, session *Session) bool {
	ids, err := requestUserIDs(r, route)
	if err != nil {
		refuse(w, r, http.StatusBadRequest, "invalid body")
		return false
	}
	session.Target = session.User
	if route.userScope() == UsersOwn {
		if !allOwnUser(ids, session.User.ID) {
			refuse(w, r, http.StatusForbidden, "forbidden")
			return false
		}
		return true
	}
	target, ok := s.adminAnyTarget(w, r, ids, session.User)
	session.Target = target
	return ok
}

// adminAnyTarget is the only decision about the user a route of that scope
// acts on. Nobody is looked up to answer a non-administrator's refusal or an
// administrator's malformed or conflicting request.
func (s *Server) adminAnyTarget(w http.ResponseWriter, r *http.Request, ids []string, caller User) (User, bool) {
	for _, id := range ids {
		if !db.ValidID(id) {
			refuse(w, r, http.StatusBadRequest, "invalid user id")
			return User{}, false
		}
	}
	if !caller.IsAdmin {
		if !allOwnUser(ids, caller.ID) {
			refuse(w, r, http.StatusForbidden, "forbidden")
			return User{}, false
		}
		return caller, true
	}
	target := ""
	for _, id := range ids {
		if target != "" && !sameUser(target, id) {
			refuse(w, r, http.StatusBadRequest, "conflicting user ids")
			return User{}, false
		}
		target = id
	}
	if target == "" || sameUser(target, caller.ID) {
		return caller, true
	}
	var user User
	err := s.Pool.QueryRow(r.Context(), "SELECT id::text, name, is_admin, last_login_at, configuration FROM users WHERE id = $1::uuid", target).
		Scan(&user.ID, &user.Name, &user.IsAdmin, &user.LastLoginAt, &user.Configuration)
	if errors.Is(err, pgx.ErrNoRows) {
		notFound(w, r)
		return User{}, false
	}
	if err != nil {
		s.fail(w, r, err)
		return User{}, false
	}
	return user, true
}
