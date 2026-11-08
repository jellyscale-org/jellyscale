//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Fields jellyfin-sdk-kotlin (Findroid, the Android TV app) declares non-null
// with no default in its generated models (AuthenticationResult's SessionInfoDto,
// PlayerStateInfo, UserDto, UserPolicy, UserConfiguration). Its JSON decoder
// throws when one is missing, and Findroid then reports the sign-in as bad
// credentials, though the server answered 200.
var (
	sdkSession = []string{"PlayableMediaTypes", "UserId", "LastActivityDate", "LastPlaybackCheckIn", "IsActive",
		"SupportsMediaControl", "SupportsRemoteControl", "HasCustomDeviceName", "SupportedCommands"}
	sdkPlayState = []string{"CanSeek", "IsPaused", "IsMuted", "RepeatMode", "PlaybackOrder"}
	sdkUser      = []string{"Id"}
	sdkPolicy    = []string{"IsAdministrator", "IsHidden", "IsDisabled", "EnableUserPreferenceAccess",
		"EnableRemoteControlOfOtherUsers", "EnableSharedDeviceControl", "EnableRemoteAccess", "EnableLiveTvManagement",
		"EnableLiveTvAccess", "EnableMediaPlayback", "EnableAudioPlaybackTranscoding", "EnableVideoPlaybackTranscoding",
		"EnablePlaybackRemuxing", "ForceRemoteSourceTranscoding", "EnableContentDeletion", "EnableContentDownloading",
		"EnableSyncTranscoding", "EnableMediaConversion", "EnableAllDevices", "EnableAllChannels", "EnableAllFolders",
		"InvalidLoginAttemptCount", "LoginAttemptsBeforeLockout", "MaxActiveSessions", "EnablePublicSharing",
		"RemoteClientBitrateLimit", "AuthenticationProviderId", "PasswordResetProviderId", "SyncPlayAccess"}
	sdkConfiguration = []string{"PlayDefaultAudioTrack", "DisplayMissingEpisodes", "GroupedFolders", "SubtitleMode",
		"DisplayCollectionsView", "EnableLocalPassword", "OrderedViews", "LatestItemsExcludes", "MyMediaExcludes",
		"HidePlayedInLatest", "RememberAudioSelections", "RememberSubtitleSelections", "EnableNextEpisodeAutoPlay"}
)

func missing(t *testing.T, where string, obj map[string]any, keys []string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := obj[k]; !ok || v == nil {
			t.Errorf("%s has no %s, which jellyfin-sdk-kotlin requires", where, k)
		}
	}
}

func object(t *testing.T, where string, v any) map[string]any {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", where, v)
	}
	return obj
}

func TestSignInAndSessionsCarryTheFieldsSDKClientsRequire(t *testing.T) {
	e := newEnv(t)
	if err := EnsureAdmin(t.Context(), e.pool, "sdk", "secret"); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, 0, "POST", "/Users/AuthenticateByName", "", map[string]string{"Username": "sdk", "Pw": "secret"},
		"Authorization", `MediaBrowser Client="Findroid", Device="phone", DeviceId="sdk-test", Version="0.15"`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in: %d %s", resp.StatusCode, body)
	}
	var auth map[string]any
	if err := json.Unmarshal(body, &auth); err != nil {
		t.Fatal(err)
	}
	session := object(t, "SessionInfo", auth["SessionInfo"])
	missing(t, "SessionInfo", session, sdkSession)
	missing(t, "SessionInfo.PlayState", object(t, "SessionInfo.PlayState", session["PlayState"]), sdkPlayState)
	user := object(t, "User", auth["User"])
	missing(t, "User", user, sdkUser)
	missing(t, "User.Policy", object(t, "User.Policy", user["Policy"]), sdkPolicy)
	missing(t, "User.Configuration", object(t, "User.Configuration", user["Configuration"]), sdkConfiguration)

	// A session is listed once the client has been seen, as after a ping.
	token, _ := auth["AccessToken"].(string)
	if resp, body := e.do(t, 0, "POST", "/Sessions/Playing/Ping", token, nil); resp.StatusCode/100 != 2 {
		t.Fatalf("ping: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, 0, "GET", "/Sessions", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sessions: %d %s", resp.StatusCode, body)
	}
	var sessions []map[string]any
	if err := json.Unmarshal(body, &sessions); err != nil || len(sessions) == 0 {
		t.Fatalf("sessions: %v %s", err, body)
	}
	for _, s := range sessions {
		where := "Sessions[" + strings.TrimSpace(s["DeviceId"].(string)) + "]"
		missing(t, where, s, sdkSession)
		missing(t, where+".PlayState", object(t, where+".PlayState", s["PlayState"]), sdkPlayState)
	}
}
