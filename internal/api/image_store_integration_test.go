//go:build integration

package api

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/db"
)

// JS-089, through the requests a client makes: a viewer's play of a film with an
// image track burnt in gets every image track of the film stored by a worker,
// with no other request and no step by the operator; a play that burns no image
// track starts no extraction. The worker is the fixture's (extractions only: no
// transcode worker runs, so a segment is never made, which these checks do not
// need). The tracks are found in the asset store without naming how they are
// stored: the film holds no text track the play extracts, so every object under
// subtitles/ is an image track's.

// assetObjects lists the keys under subtitles/ of the image film's asset store.
func (f *subtitleFixture) assetObjects(t *testing.T, library string) []string {
	t.Helper()
	uri := scalar[string](t, f.pool, "SELECT asset_store_uri FROM libraries WHERE name = $1", library)
	root := strings.TrimPrefix(uri, "file://")
	var found []string
	filepath.WalkDir(filepath.Join(root, "subtitles"), func(path string, d fs.DirEntry, err error) error { //nolint:errcheck // None yet is an empty listing.
		if err == nil && d.Type().IsRegular() && !strings.Contains(d.Name(), ".tmp") {
			found = append(found, path)
		}
		return nil
	})
	return found
}

// plays makes the requests a client makes for a transcode of the film: the
// playlists the PlaybackInfo's TranscodingUrl names, on both replicas, and
// a request for a segment that is never answered (no worker transcodes here), as
// a player does.
func (f *subtitleFixture) plays(t *testing.T, source map[string]any) {
	t.Helper()
	raw, ok := source["TranscodingUrl"].(string)
	if !ok {
		return
	}
	for replica := range f.servers {
		response, master := f.do(t, replica, "GET", raw, "", nil)
		if response.StatusCode != 200 {
			t.Fatalf("master playlist on replica %d: %d %s", replica, response.StatusCode, master)
		}
		_, rest, ok := strings.Cut(string(master), "main.m3u8?")
		if !ok {
			t.Fatalf("master playlist names no main playlist:\n%s", master)
		}
		base := raw[:strings.Index(raw, "master.m3u8")]
		mainURL := base + "main.m3u8?" + strings.Fields(rest)[0]
		response, media := f.do(t, replica, "GET", mainURL, "", nil)
		if response.StatusCode != 200 {
			t.Fatalf("main playlist on replica %d: %d %s", replica, response.StatusCode, media)
		}
		for _, line := range strings.Split(string(media), "\n") {
			if strings.HasPrefix(line, "hls1/main/") && strings.Contains(line, ".mp4") && !strings.Contains(line, "-1.mp4") {
				go func() { // Never answered here; ends with the test's servers.
					client := http.Client{Timeout: 5 * time.Second}
					if r, err := client.Get(f.servers[replica].URL + base + strings.TrimSpace(line)); err == nil {
						io.Copy(io.Discard, r.Body) //nolint:errcheck
						r.Body.Close()
					}
				}()
				break
			}
		}
	}
}

func TestAPlayThatBurnsAnImageTrackGetsEveryImageTrackOfTheFilmStored(t *testing.T) {
	f := subtitleEnv(t, "")
	f.enableTranscoding(t)
	plain := profileWith(t, false)
	for _, index := range []int{2, 3, 4} { // PGS, VobSub and DVB each: any of them is a burn, and the film has the three.
		t.Run(fmt.Sprintf("track %d chosen", index), func(t *testing.T) {
			mustExec(t, f.pool, "TRUNCATE subtitle_jobs")
			f.stopWorker()
			f.startWorker(t, 0)
			for _, o := range f.assetObjects(t, "Image") {
				t.Fatalf("the film's asset store holds %s before the play", o)
			}
			f.plays(t, f.imageInfo(t, plain, map[string]any{"SubtitleStreamIndex": index}))
			waitUntil(t, "the film's three image tracks to be stored", func() bool {
				return len(f.assetObjects(t, "Image")) >= 3 && scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs WHERE state IN ('queued', 'running')") == 0
			})
			time.Sleep(2 * time.Second) // Whatever a second request would have queued has been queued.
			if got := f.assetObjects(t, "Image"); len(got) != 3 {
				t.Errorf("the asset store holds %d objects after a play, want the film's three image tracks: %v", len(got), got)
			}
			if rows := scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs"); rows != 1 {
				t.Errorf("a play queued %d extraction jobs for one film, want one", rows)
			}
			for _, o := range f.assetObjects(t, "Image") {
				if strings.HasSuffix(o, ".sup") && index != 2 {
					t.Errorf("a play that burns track %d stored a .sup copy %s, which only a client that draws PGS is given", index, o)
				}
			}
			mustExec(t, f.pool, "DELETE FROM subtitle_jobs") // The next subtest asks again from nothing.
			f.removeObjects(t, "Image")
		})
	}
}

// removeObjects empties the film's asset store of its subtitles.
func (f *subtitleFixture) removeObjects(t *testing.T, library string) {
	t.Helper()
	for _, o := range f.assetObjects(t, library) {
		if err := os.Remove(o); err != nil {
			t.Fatal(err)
		}
	}
}

// A play that burns no image track starts no extraction: direct play, a PGS
// track the client draws (a .sup is fetched, by its own request, and a play
// does not ask for it), and a film whose track is text.
func TestAPlayThatBurnsNoImageTrackStartsNoExtraction(t *testing.T) {
	f := subtitleEnv(t, "")
	f.enableTranscoding(t)
	mustExec(t, f.pool, "TRUNCATE subtitle_jobs")
	plain, renders := profileWith(t, false), profileWith(t, true)
	f.plays(t, f.imageInfo(t, plain, nil))                                           // Direct play: no track chosen.
	f.plays(t, f.imageInfo(t, renders, map[string]any{"SubtitleStreamIndex": 2}))    // PGS drawn by the client.
	f.plays(t, f.imageInfo(t, renders, map[string]any{"MaxStreamingBitrate": 1000})) // A transcode with no track chosen.
	body := map[string]any{"DeviceProfile": plain, "MaxStreamingBitrate": 1000, "AudioStreamIndex": "1", "SubtitleStreamIndex": "2", "MediaSourceId": db.CompactID(f.text)}
	f.plays(t, firstSource(f.playbackInfo(t, 0, f.text, body))) // A text track of the other film, in a transcode.
	time.Sleep(3 * time.Second)
	if got := f.assetObjects(t, "Image"); len(got) != 0 {
		t.Errorf("a play that burns no image track stored %v", got)
	}
	if rows := scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs WHERE item_id = $1", f.image); rows != 0 {
		t.Errorf("a play that burns no image track queued %d extraction jobs for the image film", rows)
	}
}
