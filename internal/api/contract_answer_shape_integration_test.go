//go:build integration

package api

// JS-183. An answer is allowed by the contract only when its status and content type are among the ones the contract
// gives for that operation, as well as its body. These cases include the refused ones and the two discovery routes,
// which the body-only conformance check does not ask. A failure inside the server is asked separately, because it needs
// a pool that cannot answer.

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/testenv"
)

// answerNode finds one object by JSON pointer in the served document.
func answerNode(document map[string]any, pointer string) map[string]any {
	var node any = document
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		fields, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = fields[strings.NewReplacer("~1", "/", "~0", "~").Replace(token)]
	}
	fields, _ := node.(map[string]any)
	return fields
}

// answerResponse follows the response's $refs to the object that names its content types.
func answerResponse(document map[string]any, path, method, status string) map[string]any {
	pointer := "/paths/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(path) + "/" + method + "/responses/" + status
	for range 8 {
		fields := answerNode(document, pointer)
		if fields == nil {
			return nil
		}
		ref, ok := fields["$ref"].(string)
		if !ok {
			return fields
		}
		pointer = strings.TrimPrefix(ref, "#")
	}
	return nil
}

// declaredMediaTypes are the content types the contract allows for one answer.
func declaredMediaTypes(document map[string]any, path, method, status string) []string {
	response := answerResponse(document, path, method, status)
	if response == nil {
		return nil
	}
	content, _ := response["content"].(map[string]any)
	types := make([]string, 0, len(content))
	for name := range content {
		types = append(types, name)
	}
	return types
}

func checkDeclaredAnswer(t *testing.T, document map[string]any, response *http.Response, raw []byte, template, method, lookupMethod, status string) {
	t.Helper()
	if got := strconv.Itoa(response.StatusCode); got != status {
		t.Fatalf("%s %s answered %s, want %s: %.120q", method, template, got, status, raw)
	}
	types := declaredMediaTypes(document, template, strings.ToLower(lookupMethod), status)
	if len(types) == 0 {
		t.Fatalf("%s %s answering %s is not a status the contract declares", method, template, status)
	}
	got, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("%s %s answered %s with no content type: %.120q", method, template, status, raw)
	}
	for _, allowed := range types {
		if got == allowed {
			return
		}
	}
	t.Fatalf("%s %s answering %s has content type %q, want one the contract declares: %v", method, template, status, got, types)
}

func TestEveryV1AnswerHasAStatusAndContentTypeTheContractDeclares(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	path := "/jellyscale/v1/items/" + kit.item + "/playback-info"
	const pathTemplate = "/jellyscale/v1/items/{itemId}/playback-info"
	const unknownSource = "00000000000000000000000000000000"
	cases := []struct {
		name, method, path, template, token string
		body                                any
		status                              int
		lookupMethod                        string // The sent method when the operation is served, else the one that is.
	}{
		{name: "contract", method: http.MethodGet, path: "/jellyscale/v1/contract", template: "/jellyscale/v1/contract", status: http.StatusOK},
		{name: "identity", method: http.MethodGet, path: "/jellyscale/v1/server", template: "/jellyscale/v1/server", status: http.StatusOK},
		{name: "track preferences", method: http.MethodGet, path: "/jellyscale/v1/track-preferences", template: "/jellyscale/v1/track-preferences", token: kit.token, status: http.StatusOK},
		{name: "track preferences bad body", method: http.MethodPatch, path: "/jellyscale/v1/track-preferences", template: "/jellyscale/v1/track-preferences", token: kit.token, body: json.RawMessage(`{"HideOtherAudio":"yes"}`), status: http.StatusBadRequest},
		{name: "track preferences switch", method: http.MethodPatch, path: "/jellyscale/v1/track-preferences", template: "/jellyscale/v1/track-preferences", token: kit.token, body: json.RawMessage(`{"HideOtherAudio":true}`), status: http.StatusConflict},
		{name: "track preferences wrong method", method: http.MethodPut, path: "/jellyscale/v1/track-preferences", template: "/jellyscale/v1/track-preferences", token: kit.token, body: map[string]any{}, status: http.StatusMethodNotAllowed, lookupMethod: http.MethodPatch},
		{name: "configuration saved", method: http.MethodPatch, path: "/jellyscale/v1/users/configuration", template: "/jellyscale/v1/users/configuration", token: kit.token, body: map[string]any{"AudioLanguagePreference": "deu"}, status: http.StatusOK},
		{name: "configuration bad body", method: http.MethodPatch, path: "/jellyscale/v1/users/configuration", template: "/jellyscale/v1/users/configuration", token: kit.token, body: json.RawMessage(`{"SubtitleMode":"Nope"}`), status: http.StatusBadRequest},
		{name: "playback info", method: http.MethodPost, path: path, template: pathTemplate, token: kit.token, body: map[string]any{}, status: http.StatusOK},
		{name: "playback info malformed body", method: http.MethodPost, path: path, template: pathTemplate, token: kit.token, body: json.RawMessage(`[]`), status: http.StatusBadRequest},
		{name: "playback info unknown source", method: http.MethodPost, path: path, template: pathTemplate, token: kit.token, body: map[string]any{"MediaSourceId": unknownSource}, status: http.StatusOK},
		{name: "playback info another user", method: http.MethodPost, path: path, template: pathTemplate, token: kit.token, body: map[string]any{"UserId": kit.other}, status: http.StatusForbidden},
		{name: "playback info unknown item", method: http.MethodPost, path: "/jellyscale/v1/items/" + unknownSource + "/playback-info", template: pathTemplate, token: kit.token, body: map[string]any{}, status: http.StatusNotFound},
		{name: "playback info no token", method: http.MethodPost, path: path, template: pathTemplate, body: map[string]any{}, status: http.StatusUnauthorized},
		{name: "declared, not served", method: http.MethodPost, path: "/jellyscale/v1/sign-in", template: "/jellyscale/v1/sign-in", body: map[string]any{}, status: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := kit.do(t, 0, tc.method, tc.path, tc.token, tc.body)
			lookupMethod := tc.lookupMethod
			if lookupMethod == "" {
				lookupMethod = tc.method
			}
			checkDeclaredAnswer(t, document, response, raw, tc.template, tc.method, lookupMethod, strconv.Itoa(tc.status))
		})
	}
}

// A failure inside a handler is a 500 the contract lists too, with the content type it lists for it.
func TestAServerFailureHasAStatusAndContentTypeTheContractDeclares(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	pool, err := pgxpool.New(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	broken := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Name: "jellyscale", ServerID: "srv",
		Roles: []string{RoleAPI}, Log: failureLog(t)}
	server := httptest.NewServer(broken.Handler())
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/jellyscale/v1/track-preferences", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Emby-Token", "a-token-the-closed-pool-cannot-check")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw := make([]byte, 512)
	n, _ := response.Body.Read(raw)
	checkDeclaredAnswer(t, document, response, raw[:n], "/jellyscale/v1/track-preferences", http.MethodGet, http.MethodGet, "500")
}

func TestTheV1NamespaceRootAndUnknownPathAnswerJSONNotFounds(t *testing.T) {
	kit := newEnv(t)
	for _, path := range []string{"/jellyscale/v1", "/jellyscale/v1/", "/jellyscale/v1/no-such-route"} {
		t.Run(path, func(t *testing.T) {
			response, raw := kit.do(t, 0, http.MethodGet, path, kit.token, nil)
			noCookie(t, response)
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("GET %s: %d %.120q, want 404", path, response.StatusCode, raw)
			}
			if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("GET %s has content type %q, want application/json: %.120q", path, got, raw)
			}
			fields := exactKeys(t, raw, []string{"reason", "action"}, "retryAfterSeconds")
			if reason, action := stringField(t, fields, "reason"), stringField(t, fields, "action"); reason != "not-found" || action != "none" {
				t.Fatalf("GET %s says reason %q and action %q, want not-found and none", path, reason, action)
			}
		})
	}
}

func TestPlaybackInfoWithAnUnknownSourceAnswersTheNotFoundReason(t *testing.T) {
	kit := newEnv(t)
	path := "/jellyscale/v1/items/" + kit.item + "/playback-info"
	const unknownSource = "00000000000000000000000000000000"
	cases := []struct {
		name, path string
		body       any
	}{
		{"named in the body", path, map[string]any{"MediaSourceId": unknownSource}},
		{"named in the query", path + "?MediaSourceId=" + unknownSource, map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := kit.do(t, 0, http.MethodPost, tc.path, kit.token, tc.body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("POST %s: %d %.160q, want 200", tc.path, response.StatusCode, raw)
			}
			var reply map[string]json.RawMessage
			if err := json.Unmarshal(raw, &reply); err != nil {
				t.Fatal(err)
			}
			var sources []json.RawMessage
			if err := json.Unmarshal(reply["MediaSources"], &sources); err != nil || len(sources) != 0 {
				t.Fatalf("an unknown source listed %d sources: %.200q", len(sources), raw)
			}
			if reason, action := stringField(t, reply, "reason"), stringField(t, reply, "action"); reason != "not-found" || action != "none" {
				t.Fatalf("an unknown source says reason %q and action %q, want not-found and none: %.200q", reason, action, raw)
			}
		})
	}
}
