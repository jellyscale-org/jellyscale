package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
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
	reasonNothingToPlay    Reason = "nothing-to-play"
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
	reasonNothingToPlay:    "choose-another",
	reasonServerFailure:    "retry",
}

// transportReasons are the ones a version locks: an unknown route, a wrong method, a missing sign-in and a server
// failure. Reasons used only by preview capabilities may be reworded until their capability becomes stable.
var transportReasons = []Reason{reasonUnauthorized, reasonNotFound, reasonMethodNotAllowed, reasonServerFailure}

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

// patternOperation reads the method and path template the mux matched, which is the operation that answered.
func patternOperation(pattern string) (method, path string, ok bool) {
	method, path, ok = strings.Cut(pattern, " ")
	return method, path, ok
}

func reasonDeclared(reasons []string, reason Reason) bool {
	return slices.Contains(reasons, string(reason))
}

// declaredForRequest is the one gate on answers: a v1 answer may carry only a status and reason its operation declares.
// A dispatcher answer has no matched mux pattern, so every operation sharing the path is allowed to declare it; an unknown
// path has no operation and is answered not-found.
func (s *Server) declaredForRequest(r *http.Request, status int, reason Reason) bool {
	document := s.contractOf()
	method, path, matched := patternOperation(r.Pattern)
	if matched {
		return reasonDeclared(document.DeclaredReasons(method, path, strconv.Itoa(status)), reason)
	}
	foundOperation := false
	for template, methods := range document.Paths {
		if !matchV1Path(template, strings.ToLower(r.URL.Path)) {
			continue
		}
		for name := range methods {
			if !slices.Contains(contractMethods, name) {
				continue
			}
			foundOperation = true
			if reasonDeclared(document.DeclaredReasons(name, template, strconv.Itoa(status)), reason) {
				return true
			}
		}
	}
	return !foundOperation && status == http.StatusNotFound && reason == reasonNotFound
}

// undeclared logs the answer the contract did not allow and sends the one refusal every served operation declares.
func (s *Server) undeclared(w http.ResponseWriter, r *http.Request, status int, reason Reason) {
	method, path, matched := patternOperation(r.Pattern)
	if !matched {
		method, path = r.Method, r.URL.Path
	}
	s.Log.Error(undeclaredAnswerLog, "method", method, "path", path, "status", status, "reason", reason)
	refusalEnvelope(w, http.StatusInternalServerError, reasonServerFailure)
}

// logRefusal writes the operator's detail once, beside the route, status and reason. A viewer's own mistake is a warning;
// a failure inside the server is an error. The detail comes from the caller, and is never a credential or the body: only
// fixed strings reach it.
func (s *Server) logRefusal(r *http.Request, status int, reason Reason, detail string) {
	method, path, matched := patternOperation(r.Pattern)
	if !matched {
		method, path = r.Method, r.URL.Path
	}
	attrs := []any{"route", method + " " + path, "status", status, "reason", string(reason), "detail", detail}
	if status >= http.StatusInternalServerError {
		s.Log.Error("v1 refusal", attrs...)
	} else {
		s.Log.Warn("v1 refusal", attrs...)
	}
}

// refuseV1 answers a v1 request from the contract's reasons, and a Jellyfin request as it always has.
func (s *Server) refuseV1(w http.ResponseWriter, r *http.Request, status int, reason Reason, text string) {
	if !isV1Path(r.URL.Path) {
		refuse(w, r, status, text)
		return
	}
	if !s.declaredForRequest(r, status, reason) {
		s.undeclared(w, r, status, reason)
		return
	}
	s.logRefusal(r, status, reason, text)
	refusalEnvelope(w, status, reason)
}

// bodyReason gates a reason carried in a 200 body by the same contract rule as a refusal of its own. It says whether
// the caller may put the reason in its answer; on no it has already written the 500 the contract allows instead.
func (s *Server) bodyReason(w http.ResponseWriter, r *http.Request, reason Reason) (Reason, bool) {
	if s.declaredForRequest(r, http.StatusOK, reason) {
		return reason, true
	}
	s.undeclared(w, r, http.StatusOK, reason)
	return "", false
}

// matrixLogLine is the line an answer the contract does not allow leaves in the operator's log.
const undeclaredAnswerLog = "undeclared answer"

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

// checkContract stops a server whose contract disagrees with itself or with the routes it serves. The document's own
// rules hold for every replica, a stream one included: the contract is one artifact. Only the route rules belong to the
// api role, which is the one that serves them.
func (s *Server) checkContract() {
	document := s.contractOf()
	for code := range reasonActions {
		entry, ok := document.Reasons[string(code)]
		if !ok || entry.Kind != "refusal" {
			panic("the reason " + string(code) + " the server writes is not a refusal on the contract")
		}
	}
	for _, reason := range transportReasons {
		entry := document.Reasons[string(reason)]
		if entry.Action != reasonActions[reason] || !entry.Locked {
			panic("contract does not lock the transport refusal " + string(reason) + " with its kind and action")
		}
	}
	for id, capability := range document.Capabilities {
		if capability.Retires && capability.State == "stable" {
			panic("the capability " + id + " leaves at the cutover and can never be stable")
		}
	}
	capabilityUsed := map[string]bool{}
	v1Operations := 0
	for path, methods := range document.Paths {
		if !strings.HasPrefix(path, v1Prefix) {
			continue
		}
		for method, operation := range methods {
			if !slices.Contains(contractMethods, method) {
				continue
			}
			v1Operations++
			if operation.State != "declared" && operation.State != "preview" {
				panic("contract marks " + method + " " + path + " " + operation.State + ", which has no compatibility gate")
			}
			if operation.Capability != "" {
				capability, ok := document.Capabilities[operation.Capability]
				if !ok {
					panic("operation " + method + " " + path + " names the unlisted capability " + operation.Capability)
				}
				if capability.State != operation.State {
					panic("operation " + method + " " + path + " disagrees with the state of its capability")
				}
				capabilityUsed[operation.Capability] = true
			}
			for status, response := range operation.Responses {
				for _, reason := range document.ResponseReasons(response) {
					if _, ok := document.Reasons[reason]; !ok {
						panic("response " + status + " of " + method + " " + path + " declares the reason " + reason + ", which the list lacks")
					}
				}
			}
		}
	}
	// A replica with no v1 contract at all has no v1 API to describe: a stream replica may start with that document.
	if v1Operations > 0 {
		for id, capability := range document.Capabilities {
			if capability.ID != id {
				panic("the capability " + id + " says its id is " + capability.ID)
			}
			if !capabilityUsed[id] {
				panic("the capability " + id + " has no operation")
			}
			if capability.State != "declared" && capability.State != "preview" {
				panic("the capability " + id + " is " + capability.State + ", which has no compatibility gate")
			}
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
	// A capability's state is as binding as its operations': a declared one has no served operation and a preview one
	// has one. The contract's own checks above already reject a stable one and a disagreement between their states.
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
		if capability.State == "declared" && servedCapability[id] {
			panic("contract declares the capability " + id + " but the server serves it")
		}
		if capability.State == "preview" && !servedCapability[id] {
			panic("contract marks the capability " + id + " preview but the server serves none of it")
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
		if len(matched) > 0 {
			allDeclared := true
			for _, operation := range matched {
				if operation.state != "declared" {
					allDeclared = false
					break
				}
			}
			if allDeclared {
				s.refuseV1(w, r, http.StatusNotFound, reasonNotServed, "not served")
				return
			}
		}
		for _, operation := range matched {
			if operation.method != method {
				continue
			}
			if operation.state == "declared" {
				// The mux has no handler for a declared method; letting it fall through would answer the catch-all's
				// text 404 beside a served method on the same path. The contract's not-served refusal is the answer.
				s.refuseV1(w, r, http.StatusNotFound, reasonNotServed, "not served")
			} else {
				next.ServeHTTP(w, r)
			}
			return
		}
		for _, operation := range matched {
			if operation.state != "declared" {
				w.Header().Set("Allow", strings.Join(servedMethods[operation.path], ", "))
				s.refuseV1(w, r, http.StatusMethodNotAllowed, reasonMethodNotAllowed, "method not allowed")
				return
			}
		}
		s.refuseV1(w, r, http.StatusNotFound, reasonNotFound, "not found")
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
