"""The named mutants of the tests: each is one small, wrong change to the product that the
test named in its comment must fail on every run. Usage: mutant.py <name> <repo> <dir>. It writes the changed
files to <dir> and <dir>/overlay.json, which go's -overlay reads (test/mutant.sh)."""
import json
import os
import re
import sys


def read(repo, rel):
    with open(os.path.join(repo, rel)) as f:
        return f.read()


def sub(source, pattern, replacement, what, flags=0):
    """One replacement, and an error naming the anchor when it is not there to find."""
    changed, n = re.subn(pattern, replacement, source, count=1, flags=flags)
    if n != 1:
        sys.exit("the mutant no longer applies: %s (pattern %s) is not in the code" % (what, pattern))
    return changed


def signature(server):
    """The parameters of requireAuth: the text, and the names of its arguments."""
    m = re.search(r"func \(s \*Server\) requireAuth\(([^)]*)\) http\.HandlerFunc \{", server)
    if not m:
        sys.exit("the mutant no longer applies: requireAuth is not declared as func (s *Server) requireAuth(...) http.HandlerFunc")
    params = m.group(1)
    names = [p.strip().split()[0] for p in params.split(",")]
    return params, names


def empty_access(repo):
    # A new Route that declares no Access. Fails: the unit test of the declarations and every Handler().
    routes = read(repo, "internal/api/routes.go")
    routes = sub(routes, r"return \[\]Route\{\n",
                 'return []Route{\n\t\t{Method: "GET", Path: "/Mutant/Empty", Status: Stub, Role: RoleAPI, Handler: noContent},\n',
                 "the start of Routes()")
    return {"internal/api/routes.go": routes}


def other_word_access(repo):
    # Three new Routes whose Access is a word that is neither "anyone" nor "viewer": the word JS-033 adds next, a
    # misspelt "viewer" and a synonym of "anyone". A Handler() that only refuses an empty Access would serve each with
    # no guard. Fails: the unit test of the declarations (each named) and every Handler().
    routes = read(repo, "internal/api/routes.go")
    added = "".join('\t\t{Method: "GET", Path: "/Mutant/%s", Access: "%s", Status: Stub, Role: RoleAPI, Handler: noContent},\n' % (p, w)
                    for p, w in (("Admin", "admin"), ("Capital", "Viewer"), ("Public", "public")))
    routes = sub(routes, r"return \[\]Route\{\n", lambda m: "return []Route{\n" + added, "the start of Routes()")
    return {"internal/api/routes.go": routes}


def videos_file_anyone(repo):
    # GET /Videos/{itemId}/{file}, the film's bytes, declared open. Fails: TestAccess and the open-list test.
    routes = read(repo, "internal/api/routes.go")
    server = read(repo, "internal/api/server.go")
    line = re.search(r'^.*Path: "/Videos/\{itemId\}/\{file\}".*$', routes, re.M)
    if not line:
        sys.exit("the mutant no longer applies: GET /Videos/{itemId}/{file} is not in routes.go")
    text = line.group(0)
    text = re.sub(r"\s*(Access|Public|Why): [^,]*,", "", text)
    open_field = "Public: true," if re.search(r"\bPublic\s+bool", server) else 'Access: "anyone", Why: "mutant",'
    text = text.replace("Handler:", open_field + " Handler:")
    return {"internal/api/routes.go": routes.replace(line.group(0), text)}


def any_token(repo):
    # The guard lets every request with a non-empty token in. Fails: TestAccess.
    server = read(repo, "internal/api/server.go")
    params, names = signature(server)
    server = sub(server, r"(func \(s \*Server\) requireAuth\([^)]*\) http\.HandlerFunc \{\n\treturn func\(w http\.ResponseWriter, r \*http\.Request\) \{\n)",
                 '\\1\t\tif compat.ParseAuth(r).Token != "" {\n\t\t\t%s(w, r)\n\t\t\treturn\n\t\t}\n' % names[0],
                 "the start of the function requireAuth returns")
    return {"internal/api/server.go": server}


def token_memory(repo):
    # A token that was accepted is remembered in the process for 5 seconds, so a token signed out a moment ago is
    # still accepted by this process. Fails: the roles run (test/roles.sh) and the logout test of TestAccess.
    server = read(repo, "internal/api/server.go")
    params, names = signature(server)
    server = sub(server, r"func \(s \*Server\) requireAuth\(", "func (s *Server) requireAuthReal(", "the declaration of requireAuth")
    rest = ", ".join(names[1:])
    wrapper = """package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"jellyscale/internal/compat"
)

type mutantSeen struct {
	session Session
	at      time.Time
}

var mutantTokens sync.Map

func (s *Server) requireAuth(%(params)s) http.HandlerFunc {
	remember := func(w http.ResponseWriter, r *http.Request) {
		if session := sessionOf(r); session.Token != "" {
			mutantTokens.Store(session.Token, mutantSeen{session, time.Now()})
		}
		%(next)s(w, r)
	}
	real := s.requireAuthReal(remember%(comma)s%(rest)s)
	return func(w http.ResponseWriter, r *http.Request) {
		if seen, ok := mutantTokens.Load(compat.ParseAuth(r).Token); ok && time.Since(seen.(mutantSeen).at) < 5*time.Second {
			%(next)s(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, seen.(mutantSeen).session)))
			return
		}
		real(w, r)
	}
}
""" % {"params": params, "next": names[0], "rest": rest, "comma": ", " if rest else ""}
    return {"internal/api/server.go": server, "internal/api/zz_mutant.go": wrapper}


def snapshot_first(repo):
    # A Snapshot route is answered from the snapshot before Postgres is asked, so a token signed out after the last
    # refresh is still served while Postgres answers. Fails: the snapshot test of JS-125.
    server = read(repo, "internal/api/server.go")
    params, names = signature(server)
    if len(names) < 2:
        sys.exit("the mutant no longer applies: requireAuth has no parameter that says a route is a Snapshot route")
    server = sub(server, r"away := db\.Of\(s\.Pool\)\.Away\(\)", "away := db.Of(s.Pool).Away() || (%s && s.mutantHolds(info.Token))" % names[1],
                 "the line that asks whether Postgres is away")
    extra = """package api

func (s *Server) mutantHolds(token string) bool {
	s.snapshot.mu.RLock()
	defer s.snapshot.mu.RUnlock()
	return s.snapshot.sessions[token] != nil
}
"""
    return {"internal/api/server.go": server, "internal/api/zz_mutant.go": extra}


def track_preferences_unlocked(repo):
    # PATCH /jellyscale/v1/track-preferences reads the document without a lock, merges it in the process and writes
    # it whole, so two PATCHes held on one row each write what they read and the second loses the first's change.
    # Fails: both forced checks of JS-118 (TestTwoPatchesHeldOnOneRowThroughTwoProcessesAreBothKept and
    # TestTwoFirstPatchesHeldOnOneRowAreBothKept), on every run.
    source = read(repo, "internal/api/track_preferences.go")
    source = sub(source, r"err = s\.Pool\.QueryRow\(r\.Context\(\), `UPDATE users SET track_preferences = COALESCE.*?\.Scan\(&stored\)\n",
                 """var current []byte
	if err = s.Pool.QueryRow(r.Context(), "SELECT track_preferences FROM users WHERE id = $1::uuid", session.User.ID).Scan(&current); err == nil {
		merged := map[string]any{}
		json.Unmarshal([]byte(defaultTrackPreferences), &merged)
		json.Unmarshal(current, &merged)
		for name, value := range patch {
			merged[name] = value
		}
		whole, _ := json.Marshal(merged)
		err = s.Pool.QueryRow(r.Context(), "UPDATE users SET track_preferences = $2::jsonb WHERE id = $1::uuid RETURNING track_preferences", session.User.ID, string(whole)).Scan(&stored)
	}
""", "the UPDATE of patchTrackPreferences", re.S)
    source = sub(source, r"\tbody, _ := json\.Marshal\(patch\)\n", "", "the marshalling of the patch")
    return {"internal/api/track_preferences.go": source}


# The mutants of the burn-in kit, the slow-database tests, the libraries test and the stall test: each is one small, wrong
# change to the product that the tests named in its comment must fail on every run (test/mutants.sh runs them).
def cue_begins_after_the_cue(repo):
    # A run that begins mid-film opens at its cut, after the packet of a cue that began earlier and is still showing.
    # Fails: every test that looks at a picture of a run inside a cue.
    source = read(repo, "internal/transcode/ffmpeg.go")
    source = sub(source, r"(func cueStart\(image \*playback\.Stream, t float64\) \(float64, error\) \{\n)",
                 "\\1\treturn t, nil\n", "the start of cueStart")
    return {"internal/transcode/ffmpeg.go": source}


def cue_reads_only_the_later_packet(repo):
    # The picture's start is looked for in the last packet at or before the cut and in no earlier one, so a PGS picture
    # built by two packets is drawn by the later one alone. Fails: TestAMidFilmRunDrawsAPGSPictureBuiltByTwoPackets.
    source = read(repo, "internal/library/imagecues.go")
    source = sub(source, r"for i := last; i >= 0; i-- \{", "for i := last; i >= max(last, 0); i-- {", "the loop of pictureStart")
    return {"internal/library/imagecues.go": source}


def cue_opens_at_the_films_start(repo):
    # A run opens the film at its start, so it reads everything before the cut. Fails: the tests of what a run reads
    # inside a cue, and of what it reads after the film is replaced.
    source = read(repo, "internal/transcode/ffmpeg.go")
    source = sub(source, r"(func cueStart\(image \*playback\.Stream, t float64\) \(float64, error\) \{\n)",
                 "\\1\treturn 0, nil\n", "the start of cueStart")
    return {"internal/transcode/ffmpeg.go": source}


def cue_nothing_showing_opens_early(repo):
    # With no cue showing the run still opens 20 s early. Fails: TestARunWhereNoCueIsShowingReadsNoMoreThanOneWithoutTheTrack.
    source = read(repo, "internal/library/imagecues.go")
    source = sub(source, r"\topen := cut\n", "\topen := cut - 20\n", "open := cut in OpenAt")
    return {"internal/library/imagecues.go": source}


def cue_lookback_too_short(repo):
    # Where no cue times are known the run opens 2 s early, not cueLookback. Fails: the lookback draw check.
    source = read(repo, "internal/transcode/ffmpeg.go")
    source = sub(source, r"(if image\.OpenAt == nil \{\n\t\treturn t - )cueLookback(, nil)", "\\g<1>2\\2", "the look-back of cueStart")
    return {"internal/transcode/ffmpeg.go": source}


def cue_lookback_too_long(repo):
    # Where no cue times are known the run opens 30 s early, so it draws a cue the look-back is meant to miss.
    # Fails: TestAMidFilmRunOfAFilmWithNoCueTimesDrawsACueWithinTheLookBackAndMissesAnOlderOne.
    source = read(repo, "internal/transcode/ffmpeg.go")
    source = sub(source, r"(if image\.OpenAt == nil \{\n\t\treturn t - )cueLookback(, nil)", "\\g<1>30\\2", "the look-back of cueStart")
    return {"internal/transcode/ffmpeg.go": source}


def cue_whole_film_read(repo):
    # A run with an image track first reads the whole film (ffprobe over its packets), as a scan of the whole film for
    # the track does. Fails: every test of what a run reads and of how that grows with the film.
    source = read(repo, "internal/transcode/ffmpeg.go")
    source = sub(source, r"(func Command\(ctx context\.Context, input string, spec playback\.Spec, start int, dir, ass string, image \*playback\.Stream, enc encoder\) \(\*exec\.Cmd, error\) \{\n)",
                 '\\1\tif image != nil {\n\t\tffprobe(ctx, "-show_entries", "packet=pts_time", "-of", "csv=p=0", input) //nolint:errcheck\n\t}\n', "the start of Command")
    return {"internal/transcode/ffmpeg.go": source}


def cue_failed_read_is_nothing_showing(repo):
    # A failed read of the cue times is taken for a film with nothing showing. Fails:
    # TestAFailedReadOfTheCueTimesNeverLeavesASegmentWithoutTheCue.
    source = read(repo, "internal/transcode/ffmpeg.go")
    source = sub(source, r"open, err := image\.OpenAt\(t\)\n", "open, err := image.OpenAt(t)\n\tif err != nil {\n\t\treturn t, nil\n\t}\n", "the OpenAt call of cueStart")
    return {"internal/transcode/ffmpeg.go": source}


def cue_read_outlives_its_job(repo):
    # A run with an image track inside the film also reads the film on a context nothing ends: a read the track costs
    # that outlives its job. Fails: TestEveryReadOfAMidFilmRunEndsWithItsJob.
    source = read(repo, "internal/transcode/worker.go")
    source = sub(source, r'(\tif image != nil && image\.Fingerprint == "" \{\n)',
                 '\\1\t\tgo func() {\n\t\t\tif body, err := media.Open(context.Background(), source.Key, 0); err == nil {\n\t\t\t\tio.Copy(io.Discard, body) //nolint:errcheck\n\t\t\t\tbody.Close()\n\t\t\t}\n\t\t}()\n',
                 "the image track's block of encode")
    return {"internal/transcode/worker.go": source}


def cue_runs_one_at_a_time(repo):
    # Every job in the process takes one lock for its whole encode, so a run waits for the read another run holds.
    # Fails: TestTwoMidFilmRunsAtOnceOnTwoWorkersEachStayWithinOneRunsBound.
    source = read(repo, "internal/transcode/worker.go")
    source = sub(source, r"(func \(w \*Worker\) encode\(ctx context\.Context, job claimed, log \*slog\.Logger\) error \{\n)",
                 "\\1\tcueReads.Lock()\n\tdefer cueReads.Unlock()\n", "the start of encode")
    source += "\nvar cueReads sync.Mutex\n"
    return {"internal/transcode/worker.go": source}


def scan_reads_the_whole_film(repo):
    # The scan reads the whole film to learn the image tracks' packets. Fails:
    # TestTheNextScanOfAFilmTheLastReleaseScannedReadsNothingThatGrowsWithTheFilm.
    source = read(repo, "internal/library/scan.go")
    source = sub(source, r"(\tfound, err := ReadImageCues\(ctx, lib\.Media, object\.Key, object\.Size\)\n)",
                 "\tif body, drained := lib.Media.Open(ctx, object.Key, 0); drained == nil {\n\t\tio.Copy(io.Discard, body) //nolint:errcheck\n\t\tbody.Close()\n\t}\n\\1", "the ReadImageCues call of readImageCues")
    return {"internal/library/scan.go": source}


FAILED_ROW = 'w.Pool.Exec(context.WithoutCancel(ctx), "UPDATE transcode_jobs SET state = \'failed\', error = \'%s\', ended_at = now() WHERE id = $1", job.ID) //nolint:errcheck\n'


def stall_upload_ends_failed(repo):
    # The poll's upload ends the job failed when it finds the lease gone. Fails the subtests in which the poll
    # sees the stop first (the worker log says "stopping ffmpeg ... reason=upload failed").
    source = read(repo, "internal/transcode/worker.go")
    source = sub(source, r'(\t\t\t\t\tstop\("upload failed"\)\n)', lambda m: m.group(1) + "\t\t\t\t\t" + FAILED_ROW % "upload failed", "stop(\"upload failed\")")
    return {"internal/transcode/worker.go": source}


def stall_keeper_cut_ends_failed(repo):
    # The end the lease keeper gives a job (the arm of the job's context ending whose cause is the keeper's) is
    # recorded failed, not cancelled. The worker's own stall arm (stop("ffmpeg has stalled")) is unreachable after a
    # stop past the lease: the keeper finds the cut due before it sends any beat. Fails the subtests in which the
    # heartbeat (the keeper's loop) sees the stop first.
    source = read(repo, "internal/transcode/worker.go")
    source = sub(source, r'(\t\t\t\t\tstop\(cause\.Error\(\)\)\n)', lambda m: m.group(1) + "\t\t\t\t\t" + FAILED_ROW % "keeper cut", "stop(cause.Error())")
    return {"internal/transcode/worker.go": source}


def stall_poll_yields_to_keeper(repo):
    # A poll tick that comes more than a second after it fired (a stopped worker running again) is dropped. The keeper's
    # cut, due as soon as the worker runs, is then first unless the poll was inside its upload already. A job that must
    # end cancelled still does. The subtests that force the poll first by stopping the worker inside a held Put pass;
    # one that only leaves a segment waiting loses the order, and fails as a harness error.
    source = read(repo, "internal/transcode/worker.go")
    source = sub(source, r"\t\t\tcase <-poll\.C:\n", "\t\t\tcase tick := <-poll.C:\n\t\t\t\tif time.Since(tick) > time.Second {\n\t\t\t\t\tcontinue\n\t\t\t\t}\n", "the poll arm")
    return {"internal/transcode/worker.go": source}


def stall_keeper_cut_late(repo):
    # The keeper ends a job 100 ms after it finds the cut due. The subtests that force the heartbeat first by leaving
    # no finished segment for the poll to upload pass; one that leaves a segment waiting has the poll first, and fails
    # as a harness error.
    source = read(repo, "internal/transcode/lease_keeper.go")
    source = sub(source, r"(func \(k \*leaseKeeper\) giveUp\(j \*leasedJob, why error\) \{\n)", "\\1\ttime.Sleep(100 * time.Millisecond)\n", "giveUp")
    return {"internal/transcode/lease_keeper.go": source}


def keeper_connect_bound(repo):
    # The keeper gives the connection it makes the bound of one renewal (half a heartbeat), not half a lease, so a
    # connect that takes several round trips of a slow database never succeeds. Fails
    # TestASlowDatabaseSetBeforeTheClaimCostsNoJobItsLeaseOrItsWorker.
    source = read(repo, "internal/transcode/lease_keeper.go")
    source = sub(source, r"context\.WithTimeout\(context\.Background\(\), k\.w\.lease\(\)/2\)\n\t\tc, err := pgx\.ConnectConfig",
                 "context.WithTimeout(context.Background(), k.bound())\n\t\tc, err := pgx.ConnectConfig", "the keeper's connect")
    return {"internal/transcode/lease_keeper.go": source}


def keeper_renewal_two_round_trips(repo):
    # The renewal is sent in the connection's default mode, a prepare and then an execute on a connection's first use:
    # two round trips, over the renewal's bound at 1 s each. Fails TestASlowDatabaseCostsNoJobItsLeaseOrItsWorker.
    source = read(repo, "internal/transcode/lease_keeper.go")
    source = sub(source, r"\t\tpgx\.QueryExecModeExec, ids, w\.ID, w\.lease\(\)\.Seconds\(\)", "\t\tids, w.ID, w.lease().Seconds()", "the renewal's query mode")
    return {"internal/transcode/lease_keeper.go": source}


def libraries_synced_by_every_role(repo):
    # Every process syncs the libraries its config lists, whatever its roles. Fails all 18 subtests of
    # TestAProcessWithoutTheWorkerRoleWritesNothingToTheLibraries.
    source = read(repo, "cmd/jellyscale/main.go")
    source = sub(source, r"if slices\.Contains\(enabled, roleWorker\) \{\n\t\tif len\(cfg\.Libraries\) == 0", "if true {\n\t\tif len(cfg.Libraries) == 0", "the worker's library sync")
    return {"cmd/jellyscale/main.go": source}


def libraries_synced_by_the_api_role(repo):
    # Only the api role syncs besides the worker. Fails the 12 subtests with api and none of the 6 stream-only ones.
    source = read(repo, "cmd/jellyscale/main.go")
    source = sub(source, r"if slices\.Contains\(enabled, roleWorker\) \{\n\t\tif len\(cfg\.Libraries\) == 0",
                 "if slices.Contains(enabled, roleWorker) || slices.Contains(enabled, api.RoleAPI) {\n\t\tif len(cfg.Libraries) == 0", "the worker's library sync")
    return {"cmd/jellyscale/main.go": source}


MUTANTS = {"empty-access": empty_access, "other-word-access": other_word_access,
           "videos-file-anyone": videos_file_anyone, "any-token": any_token,
           "token-memory": token_memory, "snapshot-first": snapshot_first,
           "track-preferences-unlocked": track_preferences_unlocked,
           "cue-begins-after-the-cue": cue_begins_after_the_cue,
           "cue-reads-only-the-later-packet": cue_reads_only_the_later_packet,
           "cue-opens-at-the-films-start": cue_opens_at_the_films_start,
           "cue-nothing-showing-opens-early": cue_nothing_showing_opens_early,
           "cue-lookback-too-short": cue_lookback_too_short,
           "cue-lookback-too-long": cue_lookback_too_long,
           "cue-whole-film-read": cue_whole_film_read,
           "cue-failed-read-is-nothing-showing": cue_failed_read_is_nothing_showing,
           "cue-read-outlives-its-job": cue_read_outlives_its_job,
           "cue-runs-one-at-a-time": cue_runs_one_at_a_time,
           "scan-reads-the-whole-film": scan_reads_the_whole_film,
           "stall-upload-ends-failed": stall_upload_ends_failed,
           "stall-keeper-cut-ends-failed": stall_keeper_cut_ends_failed,
           "stall-poll-yields-to-keeper": stall_poll_yields_to_keeper,
           "stall-keeper-cut-late": stall_keeper_cut_late,
           "keeper-connect-bound": keeper_connect_bound,
           "keeper-renewal-two-round-trips": keeper_renewal_two_round_trips,
           "libraries-synced-by-every-role": libraries_synced_by_every_role,
           "libraries-synced-by-the-api-role": libraries_synced_by_the_api_role}

if len(sys.argv) != 4 or sys.argv[1] not in MUTANTS:
    sys.exit("usage: mutant.py <%s> <repo> <dir>" % "|".join(MUTANTS))
name, repo, out = sys.argv[1:]
overlay = {}
for rel, text in MUTANTS[name](repo).items():
    target = os.path.join(out, rel.replace("/", "_"))
    with open(target, "w") as f:
        f.write(text)
    overlay[os.path.join(repo, rel)] = target
with open(os.path.join(out, "overlay.json"), "w") as f:
    json.dump({"Replace": overlay}, f)
