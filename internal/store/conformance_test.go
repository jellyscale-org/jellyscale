package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
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

			// Delete removes one object, leaves its neighbours, and is idempotent.
			if err := s.Delete(ctx, "b/two.bin"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Open(ctx, "b/two.bin", 0); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted key still opens: %v", err)
			}
			if err := s.Delete(ctx, "b/two.bin"); err != nil {
				t.Fatalf("second delete: %v", err)
			}
			if err := s.Delete(ctx, "never/existed"); err != nil {
				t.Fatalf("delete of a missing key: %v", err)
			}
			objects, err = s.List(ctx, "")
			if err != nil || len(objects) != 1 || objects[0].Key != "a/deep/one.bin" {
				t.Fatalf("after delete: %+v %v", objects, err)
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

// SeaweedFS ends the body of one GET early when another GET of the same
// object is dropped mid-body, so dropping a read must leave the rest whole.
// ffmpeg drops reads whenever it seeks, and a transcode used to lose its input
// to this; the object has to span several of the gateway's chunks to show it.
// A Reader resumes where the cut body stopped.
func TestDroppedReadDoesNotCutAnother(t *testing.T) {
	ctx := context.Background()
	body := make([]byte, 32<<20)
	for i := range body {
		body[i] = byte(i * 31 / 7)
	}
	for name, s := range backends(t) {
		if _, ok := s.(*S3); !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if err := s.Put(ctx, "big.bin", bytes.NewReader(body)); err != nil {
				t.Fatal(err)
			}
			for round := range 100 {
				whole := NewReader(ctx, s, "big.bin", int64(len(body)))
				whole.Seek(920, io.SeekStart) //nolint:errcheck
				// The held read must be mid-body while the others are dropped.
				head := make([]byte, 1<<20)
				if _, err := io.ReadFull(whole, head); err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				for k := range 3 {
					dropped, err := s.Open(ctx, "big.bin", int64(236275+k*1000003))
					if err != nil {
						t.Fatal(err)
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						io.ReadFull(dropped, make([]byte, 1<<20)) //nolint:errcheck
						dropped.Close()
					}()
				}
				rest, err := io.ReadAll(whole)
				wg.Wait()
				whole.Close()
				got := append(head, rest...)
				if err != nil || !bytes.Equal(got, body[920:]) {
					t.Fatalf("round %d: read %d of %d bytes, err %v", round, len(got), len(body)-920, err)
				}
			}
		})
	}
}

// A worker lives for days and runs a job after job on one context; closing an
// input must not leave a goroutine waiting on that context.
func TestClosedInputsLeaveNoGoroutines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := runtime.NumGoroutine()
	for range 50 {
		_, closeInput, err := Input(ctx, remoteOnly{NewPosix(t.TempDir())}, "video", 10)
		if err != nil {
			t.Fatal(err)
		}
		closeInput()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+5 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("%d goroutines before, %d after closing 50 inputs", before, after)
	}
}

// shortStore serves bodies that end after at most cut bytes, as SeaweedFS does
// to a GET whose neighbour was dropped, reported as S3.Open reports it.
type shortStore struct {
	Store
	size, cut, opens int
	clean            bool // End the body with a plain EOF, as an empty 200 does.
}

func (s *shortStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	s.opens++
	body, err := s.Store.Open(ctx, key, offset)
	if err != nil {
		return nil, err
	}
	if s.clean {
		return struct {
			io.Reader
			io.Closer
		}{io.LimitReader(body, int64(s.cut)), body}, nil
	}
	return &whole{ReadCloser: struct {
		io.Reader
		io.Closer
	}{io.LimitReader(body, int64(s.cut)), body}, left: int64(s.size) - offset}, nil
}

func TestReaderResumesAfterAShortBody(t *testing.T) {
	ctx := context.Background()
	inner := NewPosix(t.TempDir())
	body := bytes.Repeat([]byte("0123456789"), 100)
	if err := inner.Put(ctx, "k", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	short := &shortStore{Store: inner, size: len(body), cut: 300}
	got, err := io.ReadAll(NewReader(ctx, short, "k", int64(len(body))))
	if err != nil || !bytes.Equal(got, body) || short.opens != 4 {
		t.Fatalf("read %d of %d bytes in %d opens, err %v", len(got), len(body), short.opens, err)
	}

	// A body that never delivers anything is an error, not a loop.
	short = &shortStore{Store: inner, size: len(body), cut: 0}
	if _, err := io.ReadAll(NewReader(ctx, short, "k", int64(len(body)))); !errors.Is(err, io.ErrUnexpectedEOF) || short.opens != maxResumes+1 {
		t.Fatalf("empty bodies: %d opens, err %v", short.opens, err)
	}

	// And cancellation ends it at once.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	short = &shortStore{Store: inner, size: len(body), cut: 0}
	if _, err := io.ReadAll(NewReader(cancelled, short, "k", int64(len(body)))); !errors.Is(err, context.Canceled) || short.opens != 1 {
		t.Fatalf("cancelled: %d opens, err %v", short.opens, err)
	}
}

// A body that ends cleanly short of the known size is cut too.
func TestReaderResumesAfterACleanShortBody(t *testing.T) {
	ctx := context.Background()
	inner := NewPosix(t.TempDir())
	body := bytes.Repeat([]byte("0123456789"), 100)
	if err := inner.Put(ctx, "k", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	short := &shortStore{Store: inner, cut: 300, clean: true}
	got, err := io.ReadAll(NewReader(ctx, short, "k", int64(len(body))))
	if err != nil || !bytes.Equal(got, body) || short.opens != 4 {
		t.Fatalf("read %d of %d bytes in %d opens, err %v", len(got), len(body), short.opens, err)
	}
}

// A ranged GET answered without a Content-Range is not that range, whatever
// its body: SeaweedFS answers a resumed one with 200 and nothing.
func TestS3RangeWithoutContentRangeIsAShortBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
	}))
	defer server.Close()
	t.Setenv("JELLYSCALE_S3_ACCESS_KEY", "k")
	t.Setenv("JELLYSCALE_S3_SECRET_KEY", "s")
	parsed, _ := url.Parse("s3://bucket?endpoint=" + server.URL)
	s, err := NewS3(parsed)
	if err != nil {
		t.Fatal(err)
	}
	body, err := s.Open(context.Background(), "segment", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := io.ReadAll(body); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want ErrUnexpectedEOF", err)
	}
}

// A segment's size is unknown, so it is read with the largest size there is.
func TestReaderOfUnknownSizeStopsAtTheEnd(t *testing.T) {
	ctx := context.Background()
	inner := NewPosix(t.TempDir())
	body := bytes.Repeat([]byte("0123456789"), 100)
	if err := inner.Put(ctx, "k", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	short := &shortStore{Store: inner, size: len(body), cut: 300}
	got, err := io.ReadAll(NewReader(ctx, short, "k", math.MaxInt64))
	if err != nil || !bytes.Equal(got, body) || short.opens != 4 {
		t.Fatalf("read %d of %d bytes in %d opens, err %v", len(got), len(body), short.opens, err)
	}
}

// A URI that carries credentials is refused, and the error must not repeat
// them, because it is logged.
func TestOpenRefusesCredentials(t *testing.T) {
	for _, uri := range []string{
		"s3://AKIAEXAMPLE:hunter2@bucket/p",
		"s3://hunter2@bucket/p",
		"file://user:hunter2@/media",
		"s3://bucket/p?endpoint=http://x&secret=hunter2",
		"s3://bucket/p?endpoint=http://x&access_key=hunter2",
		"s3://bucket/p?endpoint=http://x&Key=hunter2",
		"s3://bucket/p?endpoint=http://x&X-Amz-Security-Token=hunter2",
		"s3://bucket/p?endpoint=http://user:hunter2@x",
		"s3://bucket:hunter2@/%zz",
		"s3://bucket/p?region=auto;secret=hunter2",
		"s3://bucket/p?endpoint=http://x/?key=hunter2",
		"file:///media?endpoint=http%3A%2F%2Fx%3Fkey%3Dhunter2",
		"file:///media?endpoint=http%253A%252F%252Fx%253Ftoken%253Dhunter2",
		"file:///media?endpoint=http://x#key=hunter2",
		"file:///media?x=%zz&secret=hunter2",
		"file:///media?password=hunter2",
		"file:///media?passwd=hunter2",
		"file:///media?sig=hunter2",
		"s3://bucket/p?X-Amz-Signature=hunter2",
		"s3://bucket/p?X-Amz-Credential=hunter2",
		"s3://bucket/p?endpoint=http%3A%2F%2Fu%3Ahunter2%40x",
	} {
		_, err := Open(uri)
		if err == nil {
			t.Errorf("%s was accepted", uri)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error for %s repeats the secret: %v", uri, err)
		}
	}
	// Without a '//' the key is the scheme, so the scheme must not be quoted.
	if _, err := Open("hunter2:hunter3@host/bucket"); err == nil || strings.Contains(err.Error(), "hunter") || !strings.Contains(err.Error(), "s3://") {
		t.Errorf("unsupported scheme: %v", err)
	}
	for _, uri := range []string{"file:///media?region=auto", "file:///monkey/design", "s3://monkey-bucket/p?endpoint=http://s3.example.com:9000&region=auto"} {
		if parsed, err := url.Parse(uri); err != nil || refuseCredentials(parsed) != nil {
			t.Errorf("%s was refused", uri)
		}
	}
}
