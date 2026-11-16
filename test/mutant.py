"""The named mutants of the access tests (JS-125): each is one small, wrong change to internal/api that the
test named in its comment must fail on every run. Usage: mutant.py <name> <repo> <dir>. It writes the changed
files to <dir> and <dir>/overlay.json, which go's -overlay reads (test/mutant.sh)."""
import json
import os
import re
import sys


def read(repo, rel):
    with open(os.path.join(repo, rel)) as f:
        return f.read()


def sub(source, pattern, replacement, what, flags=0):
    """One replacement, and an error naming the anchor when it is not there to find."""
    changed, n = re.subn(pattern, replacement, source, count=1, flags=flags)
    if n != 1:
        sys.exit("the mutant no longer applies: %s (pattern %s) is not in the code" % (what, pattern))
    return changed


def signature(server):
    """The parameters of requireAuth: the text, and the names of its arguments."""
    m = re.search(r"func \(s \*Server\) requireAuth\(([^)]*)\) http\.HandlerFunc \{", server)
    if not m:
        sys.exit("the mutant no longer applies: requireAuth is not declared as func (s *Server) requireAuth(...) http.HandlerFunc")
    params = m.group(1)
    names = [p.strip().split()[0] for p in params.split(",")]
    return params, names


def empty_access(repo):
    # A new Route that declares no Access. Fails: the unit test of the declarations and every Handler().
    routes = read(repo, "internal/api/routes.go")
    routes = sub(routes, r"return \[\]Route\{\n",
                 'return []Route{\n\t\t{Method: "GET", Path: "/Mutant/Empty", Status: Stub, Role: RoleAPI, Handler: noContent},\n',
                 "the start of Routes()")
    return {"internal/api/routes.go": routes}


def other_word_access(repo):
    # Three new Routes whose Access is a word that is neither "anyone" nor "viewer": the word JS-033 adds next, a
    # misspelt "viewer" and a synonym of "anyone". A Handler() that only refuses an empty Access would serve each with
    # no guard. Fails: the unit test of the declarations (each named) and every Handler().
    routes = read(repo, "internal/api/routes.go")
    added = "".join('\t\t{Method: "GET", Path: "/Mutant/%s", Access: "%s", Status: Stub, Role: RoleAPI, Handler: noContent},\n' % (p, w)
                    for p, w in (("Admin", "admin"), ("Capital", "Viewer"), ("Public", "public")))
    routes = sub(routes, r"return \[\]Route\{\n", lambda m: "return []Route{\n" + added, "the start of Routes()")
    return {"internal/api/routes.go": routes}


def videos_file_anyone(repo):
    # GET /Videos/{itemId}/{file}, the film's bytes, declared open. Fails: TestAccess and the open-list test.
    routes = read(repo, "internal/api/routes.go")
    server = read(repo, "internal/api/server.go")
    line = re.search(r'^.*Path: "/Videos/\{itemId\}/\{file\}".*$', routes, re.M)
    if not line:
        sys.exit("the mutant no longer applies: GET /Videos/{itemId}/{file} is not in routes.go")
    text = line.group(0)
    text = re.sub(r"\s*(Access|Public|Why): [^,]*,", "", text)
    open_field = "Public: true," if re.search(r"\bPublic\s+bool", server) else 'Access: "anyone", Why: "mutant",'
    text = text.replace("Handler:", open_field + " Handler:")
    return {"internal/api/routes.go": routes.replace(line.group(0), text)}


def any_token(repo):
    # The guard lets every request with a non-empty token in. Fails: TestAccess.
    server = read(repo, "internal/api/server.go")
    params, names = signature(server)
    server = sub(server, r"(func \(s \*Server\) requireAuth\([^)]*\) http\.HandlerFunc \{\n\treturn func\(w http\.ResponseWriter, r \*http\.Request\) \{\n)",
                 '\\1\t\tif compat.ParseAuth(r).Token != "" {\n\t\t\t%s(w, r)\n\t\t\treturn\n\t\t}\n' % names[0],
                 "the start of the function requireAuth returns")
    return {"internal/api/server.go": server}


def token_memory(repo):
    # A token that was accepted is remembered in the process for 5 seconds, so a token signed out a moment ago is
    # still accepted by this process. Fails: the roles run (test/roles.sh) and the logout test of TestAccess.
    server = read(repo, "internal/api/server.go")
    params, names = signature(server)
    server = sub(server, r"func \(s \*Server\) requireAuth\(", "func (s *Server) requireAuthReal(", "the declaration of requireAuth")
    rest = ", ".join(names[1:])
    wrapper = """package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"jellyscale/internal/compat"
)

type mutantSeen struct {
	session Session
	at      time.Time
}

var mutantTokens sync.Map

func (s *Server) requireAuth(%(params)s) http.HandlerFunc {
	remember := func(w http.ResponseWriter, r *http.Request) {
		if session := sessionOf(r); session.Token != "" {
			mutantTokens.Store(session.Token, mutantSeen{session, time.Now()})
		}
		%(next)s(w, r)
	}
	real := s.requireAuthReal(remember%(comma)s%(rest)s)
	return func(w http.ResponseWriter, r *http.Request) {
		if seen, ok := mutantTokens.Load(compat.ParseAuth(r).Token); ok && time.Since(seen.(mutantSeen).at) < 5*time.Second {
			%(next)s(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, seen.(mutantSeen).session)))
			return
		}
		real(w, r)
	}
}
""" % {"params": params, "next": names[0], "rest": rest, "comma": ", " if rest else ""}
    return {"internal/api/server.go": server, "internal/api/zz_mutant.go": wrapper}


def snapshot_first(repo):
    # A Snapshot route is answered from the snapshot before Postgres is asked, so a token signed out after the last
    # refresh is still served while Postgres answers. Fails: the snapshot test of JS-125.
    server = read(repo, "internal/api/server.go")
    params, names = signature(server)
    if len(names) < 2:
        sys.exit("the mutant no longer applies: requireAuth has no parameter that says a route is a Snapshot route")
    server = sub(server, r"away := db\.Of\(s\.Pool\)\.Away\(\)", "away := db.Of(s.Pool).Away() || (%s && s.mutantHolds(info.Token))" % names[1],
                 "the line that asks whether Postgres is away")
    extra = """package api

func (s *Server) mutantHolds(token string) bool {
	s.snapshot.mu.RLock()
	defer s.snapshot.mu.RUnlock()
	return s.snapshot.sessions[token] != nil
}
"""
    return {"internal/api/server.go": server, "internal/api/zz_mutant.go": extra}


def track_preferences_unlocked(repo):
    # PATCH /jellyscale/v1/track-preferences reads the document without a lock, merges it in the process and writes
    # it whole, so two PATCHes held on one row each write what they read and the second loses the first's change.
    # Fails: both forced checks of JS-118 (TestTwoPatchesHeldOnOneRowThroughTwoProcessesAreBothKept and
    # TestTwoFirstPatchesHeldOnOneRowAreBothKept), on every run.
    source = read(repo, "internal/api/track_preferences.go")
    source = sub(source, r"err = s\.Pool\.QueryRow\(r\.Context\(\), `UPDATE users SET track_preferences = COALESCE.*?\.Scan\(&stored\)\n",
                 """var current []byte
	if err = s.Pool.QueryRow(r.Context(), "SELECT track_preferences FROM users WHERE id = $1::uuid", session.User.ID).Scan(&current); err == nil {
		merged := map[string]any{}
		json.Unmarshal([]byte(defaultTrackPreferences), &merged)
		json.Unmarshal(current, &merged)
		for name, value := range patch {
			merged[name] = value
		}
		whole, _ := json.Marshal(merged)
		err = s.Pool.QueryRow(r.Context(), "UPDATE users SET track_preferences = $2::jsonb WHERE id = $1::uuid RETURNING track_preferences", session.User.ID, string(whole)).Scan(&stored)
	}
""", "the UPDATE of patchTrackPreferences", re.S)
    source = sub(source, r"\tbody, _ := json\.Marshal\(patch\)\n", "", "the marshalling of the patch")
    return {"internal/api/track_preferences.go": source}


MUTANTS = {"empty-access": empty_access, "other-word-access": other_word_access,
           "videos-file-anyone": videos_file_anyone, "any-token": any_token,
           "token-memory": token_memory, "snapshot-first": snapshot_first,
           "track-preferences-unlocked": track_preferences_unlocked}

if len(sys.argv) != 4 or sys.argv[1] not in MUTANTS:
    sys.exit("usage: mutant.py <%s> <repo> <dir>" % "|".join(MUTANTS))
name, repo, out = sys.argv[1:]
overlay = {}
for rel, text in MUTANTS[name](repo).items():
    target = os.path.join(out, rel.replace("/", "_"))
    with open(target, "w") as f:
        f.write(text)
    overlay[os.path.join(repo, rel)] = target
with open(os.path.join(out, "overlay.json"), "w") as f:
    json.dump({"Replace": overlay}, f)
