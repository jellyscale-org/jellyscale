package dev.spike.pathsecret;

import android.app.Activity;
import android.content.Context;
import android.content.Intent;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.util.Log;
import android.view.SurfaceView;
import android.widget.FrameLayout;

import androidx.media3.common.MediaItem;
import androidx.media3.common.PlaybackException;
import androidx.media3.common.Player;
import androidx.media3.common.VideoSize;
import androidx.media3.datasource.DataSource;
import androidx.media3.datasource.DefaultDataSource;
import androidx.media3.exoplayer.ExoPlayer;
import androidx.media3.exoplayer.analytics.AnalyticsListener;
import androidx.media3.exoplayer.hls.HlsMediaSource;
import androidx.media3.exoplayer.source.LoadEventInfo;
import androidx.media3.exoplayer.source.MediaLoadData;

import java.io.File;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.StandardOpenOption;

/** Minimal Media3 player: one link, no credentials, commands from adb broadcasts. */
public final class PlayerActivity extends Activity {
    static final String TAG = "JS219";
    static final String COMMAND = "dev.spike.pathsecret.COMMAND";
    static PlayerActivity current;

    private ExoPlayer player;
    private String link;
    private final Handler ticker = new Handler(Looper.getMainLooper());
    private final Runnable tick = new Runnable() {
        @Override
        public void run() {
            if (player != null) {
                note("TICK state=" + player.getPlaybackState() + " pos=" + player.getCurrentPosition()
                        + " playing=" + player.isPlaying());
            }
            ticker.postDelayed(this, 5000);
        }
    };
    private PowerManager.WakeLock wakeLock;

    @Override
    protected void onCreate(Bundle bundle) {
        super.onCreate(bundle);
        SurfaceView surface = new SurfaceView(this);
        FrameLayout root = new FrameLayout(this);
        root.addView(surface, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT));
        setContentView(root);

        player = new ExoPlayer.Builder(this).build();
        player.setVideoSurfaceView(surface);
        player.setPlayWhenReady(true);
        player.addListener(new Player.Listener() {
            @Override
            public void onPlaybackStateChanged(int state) {
                note("EVENT state=" + state + " pos=" + player.getCurrentPosition()
                        + " playWhenReady=" + player.getPlayWhenReady());
            }

            @Override
            public void onIsPlayingChanged(boolean playing) {
                note("EVENT playing=" + playing + " pos=" + player.getCurrentPosition());
            }

            @Override
            public void onVideoSizeChanged(VideoSize size) {
                note("VIDEO " + size.width + "x" + size.height);
            }

            @Override
            public void onPositionDiscontinuity(Player.PositionInfo oldPosition,
                                                Player.PositionInfo newPosition, int reason) {
                note("DISCONTINUITY reason=" + reason + " from=" + oldPosition.positionMs
                        + " to=" + newPosition.positionMs);
            }

            @Override
            public void onPlayerError(PlaybackException error) {
                noteError("PLAYER_ERROR code=" + error.errorCode
                        + " name=" + error.getErrorCodeName()
                        + " message=" + error.getMessage()
                        + " cause=" + error.getCause());
            }
        });
        player.addAnalyticsListener(new AnalyticsListener() {
            @Override
            public void onLoadError(AnalyticsListener.EventTime time,
                                    LoadEventInfo info,
                                    MediaLoadData data,
                                    IOException error,
                                    boolean omitted) {
                noteError("LOAD_ERROR uri=" + info.uri + " cause=" + error);
            }
        });
        PowerManager power = (PowerManager) getSystemService(Context.POWER_SERVICE);
        if (power != null) {
            wakeLock = power.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "js219:spike");
            wakeLock.acquire(45 * 60 * 1000L);
        }
        current = this;
        ticker.post(tick);
        startLink(getIntent());
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        startLink(intent);
    }

    private void startLink(Intent intent) {
        String url = intent.getStringExtra("url");
        if (url == null || url.equals(link)) {
            return;
        }
        note("NEW_LINK " + url);
        player.stop();
        link = url;
        DataSource.Factory sources = new DefaultDataSource.Factory(this);
        MediaItem item = MediaItem.fromUri(url);
        HlsMediaSource source = new HlsMediaSource.Factory(sources).createMediaSource(item);
        player.setMediaSource(source);
        player.setPlayWhenReady(true);
        player.prepare();
    }

    void command(String action, int milliseconds) {
        if (player == null) {
            note("CMD " + action + " ignored: no player");
            return;
        }
        switch (action) {
            case "pause":
                player.setPlayWhenReady(false);
                note("CMD pause applied pos=" + player.getCurrentPosition());
                break;
            case "resume":
                player.setPlayWhenReady(true);
                note("CMD resume applied pos=" + player.getCurrentPosition());
                break;
            case "seek":
                player.seekTo(milliseconds);
                note("CMD seek applied to=" + milliseconds + " pos=" + player.getCurrentPosition());
                break;
            case "stop":
                player.stop();
                note("CMD stop applied");
                break;
            case "state":
                note("STATE state=" + player.getPlaybackState() + " pos=" + player.getCurrentPosition()
                        + " playing=" + player.isPlaying() + " link=" + link);
                break;
            default:
                note("CMD unknown " + action);
                break;
        }
    }

    void note(String message) {
        Log.i(TAG, message);
        writeState(message);
    }

    void noteError(String message) {
        Log.e(TAG, message);
        writeState(message);
    }

    private void writeState(String message) {
        File directory = getExternalFilesDir(null);
        if (directory == null) {
            return;
        }
        try {
            Files.write(new File(directory, "state.txt").toPath(),
                    (System.currentTimeMillis() + " " + message + "\n").getBytes(StandardCharsets.UTF_8),
                    StandardOpenOption.CREATE, StandardOpenOption.APPEND);
        } catch (IOException ignored) {
            Log.w(TAG, "state file unavailable");
        }
    }

    @Override
    protected void onDestroy() {
        current = null;
        ticker.removeCallbacksAndMessages(null);
        if (wakeLock != null && wakeLock.isHeld()) {
            wakeLock.release();
        }
        if (player != null) {
            player.release();
        }
        super.onDestroy();
    }
}
