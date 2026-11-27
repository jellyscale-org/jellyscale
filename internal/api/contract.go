package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"jellyscale/internal/contract"
)

// buildVersion is the version the binary was built with. The image build sets it with -ldflags -X from its version label,
// and identity reports it. A build outside the image reports the source tree it came from.
var buildVersion = "dev"

// v1Prefix is jellyscale's own namespace. Jellyfin's routes keep their text refusals; everything under this prefix
// answers them with the reason and action from the contract.
const v1Prefix = "/jellyscale/v1/"

// isV1Path includes the namespace root without its last slash, so a client that asks for the namespace itself gets
// the same refusal as one that asks for a path in it.
func isV1Path(path string) bool {
	return path == strings.TrimSuffix(v1Prefix, "/") || strings.HasPrefix(path, v1Prefix)
}

// Reason is one code on the contract's closed list. Within a version a reason is added, never removed or renamed, so a
// client generated from an older copy can decode one it does not know (criterion 3).
type Reason string

const (
	reasonUnauthorized     Reason = "unauthorized"
	reasonForbidden        Reason = "forbidden"
	reasonNotFound         Reason = "not-found"
	reasonMethodNotAllowed Reason = "method-not-allowed"
	reasonNotServed        Reason = "not-served"
	reasonInvalidBody      Reason = "invalid-body"
	reasonLanguageNotSaved Reason = "language-not-saved"
	reasonNoCapacity       Reason = "no-capacity"
	reasonServerFailure    Reason = "server-failure"
)

// reasonActions is what a viewer can do about each reason. Handler refuses to start if the contract disagrees.
var reasonActions = map[Reason]string{
	reasonUnauthorized:     "sign-in",
	reasonForbidden:        "none",
	reasonNotFound:         "none",
	reasonMethodNotAllowed: "none",
	reasonNotServed:        "none",
	reasonInvalidBody:      "none",
	reasonLanguageNotSaved: "choose-another",
	reasonNoCapacity:       "retry",
	reasonServerFailure:    "retry",
}

// reasonForStatus is the reason a handler that names no reason means by its status. A switch with no saved language
// names language-not-saved itself, since it shares 400 with a bad body.
func reasonForStatus(status int) Reason {
	switch status {
	case http.StatusBadRequest:
		return reasonInvalidBody
	case http.StatusUnauthorized:
		return reasonUnauthorized
	case http.StatusForbidden:
		return reasonForbidden
	case http.StatusNotFound:
		return reasonNotFound
	case http.StatusMethodNotAllowed:
		return reasonMethodNotAllowed
	case http.StatusConflict:
		return reasonLanguageNotSaved
	default:
		return reasonServerFailure
	}
}

// refusal is the one writer of an envelope on jellyscale's API. Its keys are exactly the ones the contract types.
func refusalEnvelope(w http.ResponseWriter, status int, reason Reason) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Reason Reason `json:"Reason"`
		Action string `json:"Action"`
	}{Reason: reason, Action: reasonActions[reason]}) //nolint:errcheck
}

// refuseV1 answers a v1 request from the contract's reasons, and a Jellyfin request as it always has.
func (s *Server) refuseV1(w http.ResponseWriter, r *http.Request, status int, reason Reason, text string) {
	if isV1Path(r.URL.Path) {
		refusalEnvelope(w, status, reason)
		return
	}
	refuse(w, r, status, text)
}

var embeddedContract = contract.Default()

func (s *Server) contractOf() *contract.Contract {
	if s.Contract != nil {
		return s.Contract
	}
	return embeddedContract
}

// getContract serves the document byte for byte, so the image carries and answers the same contract.
func (s *Server) getContract(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Write(s.contractOf().Raw) //nolint:errcheck
}

// getServer answers identity with no token. Its capabilities are the contract's, so a client never learns a capability
// this build does not describe.
func (s *Server) getServer(w http.ResponseWriter, _ *http.Request) {
	document := s.contractOf()
	ids := make([]string, 0, len(document.Capabilities))
	for id := range document.Capabilities {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	capabilities := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		capabilities = append(capabilities, map[string]string{"Id": id, "State": document.Capabilities[id].State})
	}
	writeJSON(w, map[string]any{
		"Name": s.Name, "Id": s.ServerID, "Version": buildVersion, "ApiVersion": "v1", "Capabilities": capabilities,
	})
}

// routeKey compares a served route with a contract operation: literal case and parameter names are not part of either.
var routeParameter = regexp.MustCompile(`\{[^}]*\}`)

func contractRouteKey(method, path string) string {
	return strings.ToUpper(method) + " " + routeParameter.ReplaceAllString(strings.ToLower(path), "{}")
}

var contractMethods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

// checkContract stops a server whose routes and contract disagree, and one that would serve a stable capability before
// the check that guards it exists. A stream replica serves no v1 route, so whatever the contract says there is not its
// business; it starts as it did before this release.
func (s *Server) checkContract() {
	document := s.contractOf()
	for code, action := range reasonActions {
		entry, ok := document.Reasons[string(code)]
		if !ok || entry.Kind != "refusal" || entry.Action != action {
			panic("contract does not list the refusal " + string(code) + " with its kind and action")
		}
	}
	if !slices.Contains(s.Roles, RoleAPI) {
		return
	}
	served := map[string]bool{}
	for _, route := range s.Routes() {
		if strings.HasPrefix(lowerLiterals(route.Path), v1Prefix) && slices.Contains(s.Roles, route.Role) {
			served[contractRouteKey(route.Method, route.Path)] = true
		}
	}
	// A capability's state is as binding as the state of the operations that make it: a declared one must have no
	// served operation, a preview one must have one, and a stable one needs the compatibility gate no release has yet.
	servedCapability := map[string]bool{}
	for path, methods := range document.Paths {
		if !strings.HasPrefix(path, v1Prefix) {
			continue
		}
		for method, operation := range methods {
			if !slices.Contains(contractMethods, method) {
				continue
			}
			key := contractRouteKey(method, path)
			switch operation.State {
			case "declared":
				if served[key] {
					panic("contract marks " + key + " declared but the server serves it")
				}
			case "preview":
				if !served[key] {
					panic("contract marks " + key + " preview but the server does not serve it")
				}
			default:
				panic("contract marks " + key + " " + operation.State + ", which has no compatibility gate")
			}
			if operation.Capability != "" && operation.State != "declared" {
				servedCapability[operation.Capability] = true
			}
			delete(served, key)
		}
	}
	for key := range served {
		panic("the server serves " + key + ", which the contract does not describe")
	}
	for id, capability := range document.Capabilities {
		switch capability.State {
		case "declared":
			if servedCapability[id] {
				panic("contract declares the capability " + id + " but the server serves it")
			}
		case "preview":
			if !servedCapability[id] {
				panic("contract marks the capability " + id + " preview but the server serves none of it")
			}
		default:
			panic("contract marks the capability " + id + " " + capability.State + ", which has no compatibility gate")
		}
	}
}

// v1Operation is one method of one path the contract describes, and whether it is served.
type v1Operation struct {
	method, path, state string
}

// v1Dispatch answers the v1 requests no route answers: a method a served path does not take, a declared operation, and
// an unknown path. It sits inside cors, so those answers allow any origin like every other answer. The patterns Go's mux
// registers stay the source of what ServedPatterns reports; this dispatch registers no pattern of its own.
func (s *Server) v1Dispatch(next http.Handler) http.Handler {
	operations := []v1Operation{}
	servedMethods := map[string][]string{}
	for path, methods := range s.contractOf().Paths {
		if !strings.HasPrefix(path, v1Prefix) {
			continue
		}
		for method, operation := range methods {
			if !slices.Contains(contractMethods, method) {
				continue
			}
			operations = append(operations, v1Operation{method: method, path: path, state: operation.State})
			if operation.State != "declared" {
				servedMethods[path] = append(servedMethods[path], strings.ToUpper(method))
			}
		}
	}
	for path := range servedMethods {
		slices.Sort(servedMethods[path])
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isV1Path(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		method := strings.ToLower(r.Method)
		if method == "head" {
			method = "get" // Go's mux serves HEAD wherever GET is registered.
		}
		var matched []v1Operation
		for _, operation := range operations {
			if matchV1Path(operation.path, r.URL.Path) {
				matched = append(matched, operation)
			}
		}
		for _, operation := range matched {
			if operation.method != method {
				continue
			}
			if operation.state == "declared" {
				refusalEnvelope(w, http.StatusNotFound, reasonNotServed)
			} else {
				next.ServeHTTP(w, r)
			}
			return
		}
		for _, operation := range matched {
			if operation.state != "declared" {
				w.Header().Set("Allow", strings.Join(servedMethods[operation.path], ", "))
				refusalEnvelope(w, http.StatusMethodNotAllowed, reasonMethodNotAllowed)
				return
			}
		}
		refusalEnvelope(w, http.StatusNotFound, reasonNotFound)
	})
}

// matchV1Path compares one segment at a time, leaving a contract parameter free to name any item.
func matchV1Path(template, path string) bool {
	want, got := strings.Split(strings.Trim(template, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(want) != len(got) {
		return false
	}
	for i, segment := range want {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if segment != got[i] {
			return false
		}
	}
	return true
}
