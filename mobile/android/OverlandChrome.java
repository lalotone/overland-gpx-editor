package com.wails.app;

import android.app.Activity;
import android.os.Build;
import android.view.View;
import android.view.WindowInsets;
import android.view.WindowInsetsController;
import android.webkit.JavascriptInterface;

/**
 * The two things the web UI needs from the window it is drawn into.
 *
 * Android 15+ draws every app edge to edge, and the WebView reports zero for
 * env(safe-area-inset-*), so the page asks for the system bar sizes here.
 * It also decides whether the status and navigation bar icons should be dark
 * (light theme) or light (dark theme and the splash).
 *
 * Exposes no data and no capability beyond that; navigation is already
 * confined to the app's own loopback origin.
 */
public final class OverlandChrome {
    private final Activity activity;

    OverlandChrome(Activity activity) {
        this.activity = activity;
    }

    /** System bar and cutout insets in CSS pixels, as JSON. */
    @JavascriptInterface
    public String insets() {
        View decor = activity.getWindow().getDecorView();
        WindowInsets insets = decor.getRootWindowInsets();
        float density = activity.getResources().getDisplayMetrics().density;
        int top = 0, bottom = 0, left = 0, right = 0;
        if (insets != null) {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                android.graphics.Insets bars = insets.getInsets(
                    WindowInsets.Type.systemBars() | WindowInsets.Type.displayCutout());
                top = bars.top; bottom = bars.bottom; left = bars.left; right = bars.right;
            } else {
                top = insets.getSystemWindowInsetTop(); bottom = insets.getSystemWindowInsetBottom();
                left = insets.getSystemWindowInsetLeft(); right = insets.getSystemWindowInsetRight();
            }
        }
        return "{\"top\":" + Math.round(top / density) + ",\"bottom\":" + Math.round(bottom / density)
            + ",\"left\":" + Math.round(left / density) + ",\"right\":" + Math.round(right / density) + "}";
    }

    /** Light icons over a dark UI, dark icons over a light one. */
    @JavascriptInterface
    public void setDarkChrome(boolean dark) {
        activity.runOnUiThread(() -> {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                WindowInsetsController controller = activity.getWindow().getInsetsController();
                if (controller == null) return;
                int light = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                    | WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
                controller.setSystemBarsAppearance(dark ? 0 : light, light);
            } else {
                View decor = activity.getWindow().getDecorView();
                int flags = decor.getSystemUiVisibility();
                int light = View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
                decor.setSystemUiVisibility(dark ? flags & ~light : flags | light);
            }
        });
    }
}
