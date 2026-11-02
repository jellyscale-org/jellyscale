//go:build integration

package store

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/testenv"
)

// s3Backends returns a store on each of the S3 targets test/integration.sh
// starts, JELLYSCALE_TEST_S3 entries each with a bucket in a store URI.
func s3Backends(t *testing.T) map[string]Store {
	t.Helper()
	stores := map[string]Store{}
	for _, entry := range testenv.S3(t) {
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
