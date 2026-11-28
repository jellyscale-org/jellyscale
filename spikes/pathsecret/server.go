// A stand-in origin for the path-secret playback-link spike. It serves HLS
// assets from a directory under a secret path component, needs no credential,
// logs every request, and can refuse the link as an ended playback.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

type recordingWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (writer *recordingWriter) WriteHeader(status int) {
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *recordingWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	written, err := writer.ResponseWriter.Write(data)
	writer.bytes += written
	return written, err
}

type origin struct {
	mu     sync.Mutex
	log    *os.File
	root   string
	prefix string
	end    bool
}

func (origin *origin) line(format string, args ...any) {
	origin.mu.Lock()
	defer origin.mu.Unlock()
	fmt.Fprintf(origin.log, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

func (origin *origin) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	recorder := &recordingWriter{ResponseWriter: writer}
	if request.URL.Path == "/control/end" {
		origin.mu.Lock()
		origin.end = true
		origin.mu.Unlock()
		recorder.WriteHeader(http.StatusOK)
		fmt.Fprint(recorder, "ended\n")
		origin.line("CONTROL end from %s", request.RemoteAddr)
		return
	}
	if !strings.HasPrefix(request.URL.Path, origin.prefix) {
		notFound(recorder)
		origin.logRequest(request, recorder, http.StatusNotFound)
		return
	}
	origin.mu.Lock()
	ended := origin.end
	origin.mu.Unlock()
	if ended {
		writeRefusal(recorder)
		origin.logRequest(request, recorder, http.StatusGone)
		return
	}
	relative := strings.TrimPrefix(request.URL.Path, origin.prefix)
	relative = strings.TrimPrefix(relative, "/")
	if relative == "" || strings.Contains(relative, "..") {
		notFound(recorder)
		origin.logRequest(request, recorder, http.StatusNotFound)
		return
	}
	clean := path.Clean(relative)
	file, err := os.Open(path.Join(origin.root, clean))
	if err != nil {
		notFound(recorder)
		origin.logRequest(request, recorder, http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		notFound(recorder)
		origin.logRequest(request, recorder, http.StatusNotFound)
		return
	}
	setContentType(recorder, clean)
	http.ServeContent(recorder, request, info.Name(), info.ModTime(), file)
	status := recorder.status
	if status == 0 {
		status = http.StatusOK
	}
	origin.logRequest(request, recorder, status)
}

func (origin *origin) logRequest(request *http.Request, recorder *recordingWriter, status int) {
	query := request.URL.RawQuery
	credentials := make([]string, 0, 4)
	for _, name := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"} {
		if request.Header.Get(name) != "" {
			credentials = append(credentials, strings.ToLower(name)+"=present")
		}
	}
	for _, name := range []string{"ApiKey", "api_key", "token"} {
		if request.URL.Query().Get(name) != "" {
			credentials = append(credentials, name+"=present")
		}
	}
	if len(credentials) == 0 {
		credentials = append(credentials, "none")
	}
	origin.line("REQ %s %s query=%q credentials=%s status=%d bytes=%d",
		request.Method, request.URL.Path, query, strings.Join(credentials, ","), status, recorder.bytes)
}

func setContentType(writer http.ResponseWriter, name string) {
	switch {
	case strings.HasSuffix(name, ".m3u8"):
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	case strings.HasSuffix(name, ".mp4"), strings.HasSuffix(name, ".m4s"):
		writer.Header().Set("Content-Type", "video/mp4")
	}
}

func notFound(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/plain")
	writer.WriteHeader(http.StatusNotFound)
	fmt.Fprint(writer, "not found\n")
}

func writeRefusal(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusGone)
	fmt.Fprint(writer, `{"reason":"playback-ended","action":"start-again"}`)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	dir := flag.String("dir", "assets", "directory of generated HLS files")
	secret := flag.String("secret", "spoke-secret", "path secret")
	logPath := flag.String("log", "server.log", "request log")
	flag.Parse()
	logFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()
	origin := &origin{
		log:    logFile,
		root:   *dir,
		prefix: "/media/" + *secret + "/",
	}
	origin.line("START addr=%s root=%s prefix=%s", *addr, *dir, origin.prefix)
	log.Fatal(http.ListenAndServe(*addr, origin))
}
