package testenv

import (
	"net"
	"net/http"
)

// JS-171: what a check needs to take the address the harness chose for a launched process, before that process binds it.

// Squat takes addr with a real listening socket that answers every request 204 with the header X-Squatter, as a health
// route that is not the launched process's would, and gives the socket back when stop is called.
func Squat(addr string) (stop func(), err error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Squatter", "1")
		w.WriteHeader(http.StatusNoContent)
	})}
	go server.Serve(l) //nolint:errcheck
	return func() { server.Close() }, nil
}

// ListenArg is the address a jellyscale command line (its arguments after the program's name) is told to listen on, and the
// index of that address in args; ok is false for a command line with none.
func ListenArg(args []string) (addr string, at int, ok bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--listen" {
			return args[i+1], i + 1, true
		}
	}
	return "", 0, false
}
