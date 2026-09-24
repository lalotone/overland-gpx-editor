"""Apply small, checked Android host adaptations to pinned Wails templates."""
from pathlib import Path
import shutil

root = Path(__file__).resolve().parent.parent
project = root / "build/android"
main = project / "app/src/main"
for name, dest in {
    "AndroidManifest.xml": main / "AndroidManifest.xml",
    "network_security_config.xml": main / "res/xml/network_security_config.xml",
    "file_paths.xml": main / "res/xml/file_paths.xml",
    "overland_icon.xml": main / "res/drawable/overland_icon.xml",
}.items():
    dest.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(root / "android" / name, dest)

def replace(path, old, new):
    text = path.read_text()
    if text.count(old) != 1:
        raise SystemExit(f"Wails template drift in {path}: expected one {old!r}")
    path.write_text(text.replace(old, new))

# Wails pins AGP 8.7.3, whose lint parser rejects Java 25. Keep its 8.x DSL
# while updating lint/D8 for Java 25 and compileSdk 36.
replace(project / "build.gradle", "version '8.7.3'", "version '8.13.2'")

gradle = project / "app/build.gradle"
replace(gradle, 'applicationId "com.wails.app"', 'applicationId "co.overland.mobile"')
replace(gradle, 'compileSdk 35', 'compileSdk 36')
replace(gradle, 'targetSdk 35', 'targetSdk 36')
replace(gradle, 'minSdk 21', 'minSdk 26')
replace(gradle, "abiFilters 'arm64-v8a', 'x86_64'", "abiFilters 'arm64-v8a'")

# Wails' share API currently only shares text. Extend it with a constrained
# FileProvider attachment under our export directory, using the same JNI API.
bridge = main / "java/com/wails/app/WailsBridge.java"
replace(bridge, 'String text = opts.optString("text", "");', '''String path = opts.optString("path", "");
                if (!path.isEmpty()) {
                    java.io.File file = new java.io.File(path);
                    java.io.File exports = new java.io.File(activity.getFilesDir(), "exports");
                    if (!file.getCanonicalPath().startsWith(exports.getCanonicalPath() + java.io.File.separator)) return;
                    android.net.Uri uri = androidx.core.content.FileProvider.getUriForFile(activity, activity.getPackageName() + ".fileprovider", file);
                    Intent attachment = new Intent(Intent.ACTION_SEND);
                    attachment.setType("application/gpx+xml");
                    attachment.putExtra(Intent.EXTRA_STREAM, uri);
                    attachment.setClipData(android.content.ClipData.newRawUri("GPX track", uri));
                    attachment.addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
                    activity.startActivity(Intent.createChooser(attachment, "Share GPX"));
                    return;
                }
                String text = opts.optString("text", "");''')

# Native interfaces must never be exposed to a page from an external origin.
activity = main / "java/com/wails/app/MainActivity.java"
replace(activity, 'private WebView webView;', 'private WebView webView;\n    private String overlandOrigin;')
replace(activity, 'webView.setWebViewClient(new WebViewClient() {', '''webView.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                android.net.Uri uri = request.getUrl();
                String origin = uri.getScheme() + "://" + uri.getAuthority();
                if (overlandOrigin == null && "http".equals(uri.getScheme()) && "127.0.0.1".equals(uri.getHost())
                        && "/mobile/start".equals(uri.getPath()) && uri.getQueryParameter("token") != null) {
                    overlandOrigin = origin;
                }
                boolean local = origin.equals(overlandOrigin)
                    || (overlandOrigin == null && "https://wails.localhost".equals(origin));
                if (local) return false;
                if ("https".equals(uri.getScheme())) startActivity(new Intent(Intent.ACTION_VIEW, uri));
                return true;
            }''')
replace(activity, 'if (webView != null && webView.canGoBack()) {', '''if (webView != null && webView.getUrl() != null && webView.getUrl().startsWith("http://127.0.0.1:")) {
            webView.evaluateJavascript("!window.dispatchEvent(new Event('overland:back', {cancelable:true}))", result -> { if ("false".equals(result)) moveTaskToBack(true); });
        } else if (webView != null && webView.canGoBack()) {''')

replace(activity, '''byte[] buf = new byte[64 * 1024];
                int n;
                while ((n = in.read(buf)) > 0) {
                    os.write(buf, 0, n);
                }''', '''byte[] buf = new byte[64 * 1024];
                int n;
                long copied = 0;
                while ((n = in.read(buf)) > 0) {
                    copied += n;
                    if (copied > 16L * 1024 * 1024) {
                        out.delete();
                        runOnUiThread(() -> android.widget.Toast.makeText(this, "GPX exceeds 16 MiB", android.widget.Toast.LENGTH_LONG).show());
                        throw new java.io.IOException("GPX exceeds import limit");
                    }
                    os.write(buf, 0, n);
                }''')

foreground = main / "java/com/wails/app/WailsForegroundService.java"
replace(foreground, 'public static final String ACTION_START', '''@Override
    public void onTimeout(int startId, int fgsType) { stopSelf(); }

    public static final String ACTION_START''')
