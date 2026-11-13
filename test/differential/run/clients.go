package main

// The sweep of compat/clients.yaml (JS-070): every request a client sends on its main paths is
// sent to the running jellyscale, over a real scan and a signed-in user, and must answer 2xx.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"

	"jellyscale/compat/conform"
)

var placeholder = regexp.MustCompile(`\{[A-Za-z]+\}`)

// fillClient puts what this server has where a listed request names it: ids in the route,
// the query and the body. A route may spell the same id with another name, so {seriesId} and
// {itemId} are what the fixtures call them.
func (s *server) fillClient(text string) (string, error) {
	var missing string
	out := placeholder.ReplaceAllStringFunc(text, func(name string) string {
		value := ""
		switch name {
		case "{userId}":
			value = s.userID
		case "{userName}":
			value = "admin"
		case "{password}":
			value = s.password
		case "{imageType}":
			value = "Primary"
		case "{imageIndex}":
			value = "0"
		case "{itemId}", "{mediaSourceId}":
			value = s.byKey["Movie/Alpha Film (2021)"]
		case "{seriesId}":
			value = s.byKey["Series/Delta Show"]
		case "{parentId}":
			value = s.byKey["CollectionFolder/Movies"]
		default: // a parameter only the route spells, such as {preferenceId}: any name does
			value = "usersettings"
		}
		if value == "" {
			missing = name
		}
		return value
	})
	if missing != "" {
		return "", fmt.Errorf("%s has nothing for %s", s.name, missing)
	}
	return out, nil
}

// request is the path with its query and the body a listed request is sent as to this server.
func (s *server) clientRequest(r conform.ClientRequest) (path, body string, err error) {
	if path, err = s.fillClient(r.Route); err != nil {
		return "", "", err
	}
	query := url.Values{}
	for name, values := range r.Query {
		for _, v := range values {
			if v, err = s.fillClient(v); err != nil {
				return "", "", err
			}
			query.Add(name, v)
		}
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	body, err = s.fillClient(r.Body)
	return path, body, err
}

// sweep sends each listed request that served accepts and returns how many it sent and what failed,
// each as the client, the path, the method, the route and what was wrong. A request that answers
// other than 2xx is excused only if reference, asked the same, gave the same status. Each answer
// goes through s.do, so it is held to the schema too. A sweep that sent nothing is itself a failure.
func (s *server) sweep(clients conform.Clients, served func(method, route string) bool, reference *server) (sent int, failures []string) {
	for _, client := range clients {
		for _, path := range conform.PathNames {
			for _, r := range client.Paths[path] {
				if !served(r.Method, r.Route) {
					continue
				}
				sent++
				where := fmt.Sprintf("%s %s %s %s", client.Name, path, r.Method, r.Route)
				line, body, err := s.clientRequest(r)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", where, err))
					continue
				}
				s.label = where
				status, data, err := s.do(r.Method, line, body)
				s.label = ""
				if err == nil && status < 300 && r.Route == "/Users/AuthenticateByName" {
					s.takeToken(data) // signing in again from the same device ends the token of the first
				}
				switch {
				case err != nil:
					failures = append(failures, fmt.Sprintf("%s: %v", where, err))
				case status < 200 || status > 299:
					if reference != nil && reference.answers(r, status) {
						continue
					}
					failures = append(failures, fmt.Sprintf("%s: answered %d, want 2xx (%s)", where, status, line))
				}
			}
		}
	}
	if sent == 0 {
		failures = append(failures, "no listed request is one jellyscale serves, so nothing was sent")
	}
	return sent, failures
}

// answers says whether this server, asked a listed request, gives that status. Its own answer is not held to the schema.
func (s *server) answers(r conform.ClientRequest, status int) bool {
	line, body, err := s.clientRequest(r)
	if err != nil {
		return false
	}
	got, _, _, err := s.send(r.Method, line, body)
	return err == nil && got == status
}

// takeToken keeps the token of a sign-in answer.
func (s *server) takeToken(answer []byte) {
	var auth struct{ AccessToken string }
	if json.Unmarshal(answer, &auth) == nil && auth.AccessToken != "" {
		s.token = auth.AccessToken
	}
}
