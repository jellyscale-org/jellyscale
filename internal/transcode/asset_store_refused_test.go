package transcode

import (
	"context"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// refusingAssets refuses every open, as an asset store that is down does.
type refusingAssets struct{ store.Store }

func (refusingAssets) Open(context.Context, string, int64) (io.ReadCloser, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// The asset store's refusal of a read fails the job and the recorded error names
// that store, for the track's read and for the fonts' read alike.
func TestAnAssetStoreRefusalNamesTheStoreAndFailsTheJob(t *testing.T) {
	const uri = "s3://assets-bucket?endpoint=http://assets.invalid"
	src := items.Source{AssetStoreURI: uri}
	reads := map[string]func() error{
		"a subtitle track": func() error {
			_, err := FetchSubtitle(context.Background(), nil, nil, refusingAssets{}, "item", src, 0)
			return err
		},
		"the film's fonts": func() error {
			_, err := FetchFonts(context.Background(), nil, nil, refusingAssets{}, "item", src)
			return err
		},
		"a font": func() error {
			return copyFont(context.Background(), refusingAssets{}, "sum", t.TempDir()+"/f", uri)
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			state, reason := endState(context.Background(), read())
			if state != "failed" {
				t.Errorf("ended %s, want failed", state)
			}
			if !strings.Contains(reason, uri) {
				t.Errorf("recorded error %q does not hold the asset store's URI", reason)
			}
		})
	}
}
