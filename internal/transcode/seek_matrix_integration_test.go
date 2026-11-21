//go:build integration

package transcode

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A DTS core track is never what stands in for DTS-HD MA: the probe tells them apart, and the kit
// refuses a film whose probe says otherwise (filmOf). This holds the kit's DTS films to it, on short films.
func TestTheProbeTellsDTSFromDTSHDMA(t *testing.T) {
	films := newSeekFilms(t)
	seen := map[string]string{}
	for _, a := range seekAudios() {
		if a.codec != "dts" {
			continue
		}
		media := films.filmOf(t, a, 20) // Fails unless the probe says the row's codec, profile and channels.
		_, profile, channels := probeAudio(t, filepath.Join(media, "film.mkv"))
		seen[a.name] = profile
		if a.vector == "" && profile == "DTS-HD MA" {
			t.Errorf("%s (a core track) probes as DTS-HD MA", a.name)
		}
		if a.vector != "" && (profile != "DTS-HD MA" || channels != a.channels) {
			t.Errorf("%s probes as %q with %d channels", a.name, profile, channels)
		}
	}
	if len(seen) != 5 {
		t.Errorf("the matrix has %d DTS films (%v), want the core in stereo and 5.1 and DTS-HD MA in 7.1 and 5.1 (twice)", len(seen), seen)
	}
}

// seekReach is how far past seekSegment the helper and failover checks may land: the reference holds that many segments and two more.
const seekReach = 40

// A seek past half of a film of more than 30 minutes that is re-encoded gets its segment, whatever the film's audio: answered,
// by one job that began at the segment and did not fail; with picture and sound from its first frame, which falls where a run from
// the film's start puts it; and the same seek gets its segment through a helper's chunk and through a failover. The matrix is each
// audio codec the dogfood's library holds, in stereo and 5.1 (DTS-HD MA in 7.1 and 5.1 and, in 5.1, with a damaged head: see
// seekAudios), each with and without an image subtitle burnt in, and each of the five that a client may take as it is also with the sound copied: 34 films re-encoded
// and 10 more with their sound copied, each of 30 minutes and 10 seconds. A row is one top-level test, so that test/shard.sh deals
// the rows to the shards and the row's film is made once for its seek, helper and failover checks.
func aSeekGetsItsSegment(t *testing.T, row string) {
	films := newSeekFilms(t)
	a := seekRow(t, row)
	s := films.rig(t, films.film(t, a), a)
	for _, copied := range []bool{false, true} {
		if copied && !a.copyable {
			continue
		}
		for _, burn := range []bool{false, true} {
			name := map[bool]string{false: "plain", true: "burnt"}[burn]
			if copied {
				name += "-copied"
			}
			t.Run(name, func(t *testing.T) {
				s.apply(a, copied, burn)
				end := seekSegment + 2
				if !copied {
					end = seekSegment + seekReach + 3
				}
				reference := s.fromTheStart(t, seekSegment-1, end, t.TempDir())
				t.Run("seek", func(t *testing.T) {
					checkSegment(t, s.seekTo(t, seekSegment), reference, seekSegment)
				})
				if copied {
					return
				}
				t.Run("through a helper's chunk", func(t *testing.T) { aSeekThroughAHelper(t, s, reference) })
				t.Run("through a failover", func(t *testing.T) { aSeekThroughAFailover(t, s, reference) })
			})
		}
	}
}

func TestASeekGetsItsSegment_aac_2(t *testing.T)          { aSeekGetsItsSegment(t, "aac-2") }
func TestASeekGetsItsSegment_aac_6(t *testing.T)          { aSeekGetsItsSegment(t, "aac-6") }
func TestASeekGetsItsSegment_ac3_2(t *testing.T)          { aSeekGetsItsSegment(t, "ac3-2") }
func TestASeekGetsItsSegment_ac3_6(t *testing.T)          { aSeekGetsItsSegment(t, "ac3-6") }
func TestASeekGetsItsSegment_eac3_2(t *testing.T)         { aSeekGetsItsSegment(t, "eac3-2") }
func TestASeekGetsItsSegment_eac3_6(t *testing.T)         { aSeekGetsItsSegment(t, "eac3-6") }
func TestASeekGetsItsSegment_dts_2(t *testing.T)          { aSeekGetsItsSegment(t, "dts-2") }
func TestASeekGetsItsSegment_dts_6(t *testing.T)          { aSeekGetsItsSegment(t, "dts-6") }
func TestASeekGetsItsSegment_dtshdma_8(t *testing.T)      { aSeekGetsItsSegment(t, "dtshdma-8") }
func TestASeekGetsItsSegment_truehd_2(t *testing.T)       { aSeekGetsItsSegment(t, "truehd-2") }
func TestASeekGetsItsSegment_truehd_6(t *testing.T)       { aSeekGetsItsSegment(t, "truehd-6") }
func TestASeekGetsItsSegment_flac_2(t *testing.T)         { aSeekGetsItsSegment(t, "flac-2") }
func TestASeekGetsItsSegment_flac_6(t *testing.T)         { aSeekGetsItsSegment(t, "flac-6") }
func TestASeekGetsItsSegment_opus_2(t *testing.T)         { aSeekGetsItsSegment(t, "opus-2") }
func TestASeekGetsItsSegment_opus_6(t *testing.T)         { aSeekGetsItsSegment(t, "opus-6") }
func TestASeekGetsItsSegment_dtshdma_6(t *testing.T)      { aSeekGetsItsSegment(t, "dtshdma-6") }
func TestASeekGetsItsSegment_dtshdma_6_head(t *testing.T) { aSeekGetsItsSegment(t, "dtshdma-6-head") }

// The rows above are the matrix: a row added to seekAudios without a test function fails here, and so does a function for no row.
func TestEveryRowOfTheSeekMatrixHasATest(t *testing.T) {
	source := string(readFile(t, "seek_matrix_integration_test.go"))
	rows := map[string]bool{}
	for _, a := range seekAudios() {
		rows[a.name] = true
		if !strings.Contains(source, "{ aSeekGetsItsSegment(t, \""+a.name+"\") }") {
			t.Errorf("row %s has no TestASeekGetsItsSegment_ function", a.name)
		}
	}
	for _, m := range regexp.MustCompile(`aSeekGetsItsSegment\(t, "([^"]+)"\) }`).FindAllStringSubmatch(source, -1) {
		if !rows[m[1]] {
			t.Errorf("a TestASeekGetsItsSegment_ function names row %s, which the kit has not", m[1])
		}
	}
	if len(rows) != 17 {
		t.Errorf("the matrix has %d rows, want 17 (34 films, with a burnt image subtitle or not)", len(rows))
	}
}
