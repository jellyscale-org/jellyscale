package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// backends returns every store to test. test/integration.sh sets S3 targets as
// JELLYSCALE_TEST_S3 entries, comma-separated, each a store URI with a bucket.
func backends(t *testing.T) map[string]Store {
	t.Helper()
	stores := map[string]Store{"posix": NewPosix(t.TempDir())}
	for _, entry := range strings.Split(os.Getenv("JELLYSCALE_TEST_S3"), ",") {
		if entry == "" {
			continue
		}
		name, uri, _ := strings.Cut(entry, "=")
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		t.Setenv("JELLYSCALE_S3_ACCESS_KEY", query.Get("key"))
		t.Setenv("JELLYSCALE_S3_SECRET_KEY", query.Get("secret"))
		parsed.Path = "/" + fmt.Sprintf("conformance-%d", time.Now().UnixNano())
		s3Store, err := NewS3(parsed)
		if err != nil {
			t.Fatal(err)
		}
		stores[name] = s3Store
	}
	return stores
}

func TestConformance(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			body := bytes.Repeat([]byte("0123456789"), 1000)
			for _, key := range []string{"b/two.bin", "a/deep/one.bin"} {
				if err := s.Put(ctx, key, bytes.NewReader(body)); err != nil {
					t.Fatal(err)
				}
			}

			objects, err := s.List(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(objects) != 2 || objects[0].Key != "a/deep/one.bin" || objects[1].Key != "b/two.bin" {
				t.Fatalf("listing: %+v", objects)
			}
			if objects[0].Size != int64(len(body)) || objects[0].ETag == "" {
				t.Fatalf("metadata: %+v", objects[0])
			}

			scoped, err := s.List(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if len(scoped) != 1 {
				t.Fatalf("prefix listing: %+v", scoped)
			}

			stream, err := s.Open(ctx, "b/two.bin", 9995)
			if err != nil {
				t.Fatal(err)
			}
			tail, err := io.ReadAll(stream)
			stream.Close()
			if err != nil || string(tail) != "56789" {
				t.Fatalf("offset read: %q %v", tail, err)
			}

			if _, err := s.Open(ctx, "missing", 0); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing key: %v", err)
			}

			if err := s.Put(ctx, "b/two.bin", strings.NewReader("short")); err != nil {
				t.Fatal(err)
			}
			objects, err = s.List(ctx, "b")
			if err != nil {
				t.Fatal(err)
			}
			if objects[0].Size != 5 {
				t.Fatalf("overwrite: %+v", objects[0])
			}
		})
	}
}

func TestServeContentOverReader(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			body := bytes.Repeat([]byte("abcdefghij"), 2000)
			if err := s.Put(ctx, "media.bin", bytes.NewReader(body)); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reader := NewReader(r.Context(), s, "media.bin", int64(len(body)))
				defer reader.Close()
				w.Header().Set("ETag", `"v1"`)
				w.Header().Set("Content-Type", "video/mp4")
				http.ServeContent(w, r, "", time.Time{}, reader)
			}))
			defer server.Close()

			get := func(method string, headers map[string]string) (*http.Response, []byte) {
				request, _ := http.NewRequest(method, server.URL, nil)
				for k, v := range headers {
					request.Header.Set(k, v)
				}
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				data, _ := io.ReadAll(response.Body)
				return response, data
			}

			response, data := get("GET", nil)
			if response.StatusCode != 200 || !bytes.Equal(data, body) {
				t.Fatalf("full: %d %d", response.StatusCode, len(data))
			}
			response, data = get("GET", map[string]string{"Range": "bytes=10-19"})
			if response.StatusCode != 206 || !bytes.Equal(data, body[10:20]) {
				t.Fatalf("range: %d %q", response.StatusCode, data)
			}
			response, data = get("GET", map[string]string{"Range": "bytes=-5"})
			if response.StatusCode != 206 || !bytes.Equal(data, body[len(body)-5:]) {
				t.Fatalf("suffix: %d %q", response.StatusCode, data)
			}
			response, data = get("GET", map[string]string{"Range": "bytes=19000-", "If-Range": `"v1"`})
			if response.StatusCode != 206 || !bytes.Equal(data, body[19000:]) {
				t.Fatalf("if-range match: %d %d", response.StatusCode, len(data))
			}
			response, _ = get("GET", map[string]string{"Range": "bytes=10-19", "If-Range": `"stale"`})
			if response.StatusCode != 200 {
				t.Fatalf("if-range stale should return everything: %d", response.StatusCode)
			}
			response, data = get("HEAD", nil)
			if response.StatusCode != 200 || len(data) != 0 || response.ContentLength != int64(len(body)) {
				t.Fatalf("head: %d %d", response.StatusCode, response.ContentLength)
			}
			response, _ = get("GET", map[string]string{"Range": "bytes=99999999-"})
			if response.StatusCode != 416 {
				t.Fatalf("out of range: %d", response.StatusCode)
			}
		})
	}
}

func TestHeadDoesNotTouchStore(t *testing.T) {
	s := &countingStore{Store: NewPosix(t.TempDir())}
	request := httptest.NewRequest("HEAD", "/", nil)
	recorder := httptest.NewRecorder()
	reader := NewReader(context.Background(), s, "absent", 100)
	defer reader.Close()
	recorder.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(recorder, request, "", time.Time{}, reader)
	if recorder.Code != 200 || s.opens != 0 {
		t.Fatalf("code %d, opens %d", recorder.Code, s.opens)
	}
}

type countingStore struct {
	Store
	opens int
}

func (c *countingStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	c.opens++
	return c.Store.Open(ctx, key, offset)
}

func TestInputServesRangesAndRejectsOtherPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := NewPosix(t.TempDir())
	if err := inner.Put(ctx, "video", strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	input, closeInput, err := Input(ctx, remoteOnly{inner}, "video", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer closeInput()

	request, _ := http.NewRequest("GET", input, nil)
	request.Header.Set("Range", "bytes=4-6")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if string(data) != "456" {
		t.Fatalf("got %q", data)
	}

	parsed, _ := url.Parse(input)
	parsed.Path = "/guess"
	other, err := http.Get(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	other.Body.Close()
	if other.StatusCode != 404 {
		t.Fatalf("got %d", other.StatusCode)
	}
}

type remoteOnly struct{ Store }

func (remoteOnly) LocalPath(string) (string, bool) { return "", false }
