package dev.spike.pathsecret;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.util.Log;

/** Adb-driven commands, so a 30-minute pause needs no UI interaction. */
public final class CommandReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        String action = intent.getStringExtra("action");
        int milliseconds = intent.getIntExtra("ms", 45000);
        PlayerActivity activity = PlayerActivity.current;
        if (activity == null) {
            Log.w(PlayerActivity.TAG, "CMD " + action + " ignored: no activity");
            return;
        }
        Log.i(PlayerActivity.TAG, "CMD received " + action);
        activity.command(action, milliseconds);
    }
}
