# Overland for Android

A separate, touch-first frontend around the existing Overland backend and GPX
libraries. The Android app embeds Broom and routes locally. The desktop frontend
continues to use its existing entry point.

## Interface

- **Explore:** full-screen map, place search at the top, fuel/water/camp overlays
  at the bottom. Map layers, location and track fitting are floating controls.
- **Plan:** map-first by default, with a round add-point button at the bottom
  right and route-info button on the left. Tap or pull up the route-points handle to reveal a panel with pinned
  Road/Dirt/Trail/Enduro profiles and save/share actions; only the point list
  scrolls. Drag numbered map controls to adjust the route. An info button opens
  distance, elevation and moving-time details over the map.
- **Library:** import GPX, reopen saved tracks, edit with undo, reverse, crop,
  simplify, split, day-stage, refresh elevation and add waypoints. Multi-track
  inputs are presented as separate selectable tracks rather than silently merged.
  Each saved track has a trash button to delete its GPX file after confirmation.
- **Offline:** a split map overlay downloads the visible area or opens a region
  browser. The browser has City, Region/comunidad and Country views, a prominent
  Downloaded section, in-use badges, saved map areas and explicit partial states.
  Selecting a region shows separate progress for routing, vector maps, elevation,
  fuel prices, fuel stations, water and campsites. City search uses the shared
  Nominatim client; cities use their own map bounds plus a covering routing extract.
  Area downloads check routing
  coverage and the pack estimate, then prepares the matching Broom region and a
  vector-map/elevation/POI/fuel pack. Its progress turns into a stop-downloads
  control while jobs run. Views outside one covering region or provider limits
  need to be narrowed. Map packs cover zooms 5–14; vector sources can overzoom
  their native tiles. Routing downloads the provider's complete regional extract,
  which can be much larger than the visible map area.

Regional downloads process up to 100,000 resources in bounded batches of 128,
with four vector-tile workers under the shared four-request provider limit. One durable pack owns every batch, so later batches
cannot evict earlier downloaded tiles. Ordinary desktop trip packs keep their
10,000-resource limit. Region manifests checkpoint batches and always flush their
terminal state; interrupted downloads can reuse existing cached resources.

Elevation (four workers), maps (four workers) and auxiliary data download in
parallel, under the shared provider limits. If the response cache reaches its
distinct-key admission rate, packs wait cancellably for the next minute window
without fetching those responses again or charging their byte budget twice.
Explicit byte-budgeted packs have a separate bounded allowance of up to 4,000
new keys/minute; passive requests retain their up-to-1,000 allowance. Actual
byte/entry quota exhaustion still stops the pack. Completed resource rows show
Downloaded immediately while other pack stages continue.

Map progress distinguishes cached resources, newly downloaded response bodies and
responses checked online without downloading the body again. Byte counters are
labelled as storage added, not network traffic. Retry progress includes checking
and pinning cached tiles. Saved packs retain their area/city/region/country kind
and show their bounding coordinates; older packs show their saved extent without
guessing that a regional name means whole-region coverage.

Routing progress polling does not inspect artifact contents. Broom 0.7's advisory
cache summary exposes whether its byte count is known, stale and when measured;
explicit verified inventory remains separate for storage management.

Large areas still respect storage and provider limits. In particular, the app
does not turn a large region into tiled Overpass harvesting: unsupported broad
place searches are marked **Provider limit** while map/elevation downloads proceed.
Search/download city areas for fresh places. It does not label a partial
city-sized map pack as a fully downloaded country. Native vector-source maximum
zooms are respected when preparing map packs.

The Online/Offline pill switches the backend's strict cache-only mode. Native
exports use Android's share sheet; opening a document uses the system picker.

Drafts are automatically saved to app-private storage, including planner controls
and the current editor document. They survive process restarts and APK updates.
Clear/reset is explicit. Library saves preserve filenames; imported files remain
create-only. Native imports and draft/share requests are limited to 16 MiB.

## Build an APK

The build script currently targets a **Linux x86-64 host and ARM64 Android**.
It uses Wails **v3.0.0-beta.25**, Go 1.27+, npm, Python 3, a current JDK, Android
SDK platform 36 and NDK r27c. Wails' generated Android host remains experimental.

From the repository root:

```sh
npm ci
sdkmanager 'platforms;android-36' 'build-tools;35.0.0' 'ndk;27.2.12479018'
export ANDROID_HOME="$HOME/Android/Sdk"
# Optional when the NDK is installed somewhere else:
# export ANDROID_NDK_HOME=/path/to/android-ndk-r27c
bash mobile/build-android.sh debug
```

Output: **`mobile/bin/overland-debug.apk`**.

```sh
adb install -r mobile/bin/overland-debug.apk
adb shell am start -n co.overland.mobile/com.wails.app.MainActivity
```

Use `install -r`, not uninstall/install: uninstalling removes the library and
downloaded regions. The application ID is `co.overland.mobile`.

`bash mobile/build-android.sh release` builds a production-mode APK. Wails uses
the Android debug keystore unless `ANDROID_KEYSTORE_FILE`,
`ANDROID_KEYSTORE_PASSWORD`, `ANDROID_KEY_ALIAS`, and `ANDROID_KEY_PASSWORD`
are supplied. Store submission/AAB packaging is not part of this build target.

The script installs its pinned Wails CLI under `mobile/bin/tools`, generates the
Android shell under `mobile/build`, applies the checked adaptations in
`android/configure.py`, builds the mobile frontend, compiles `libwails.so`, and
runs Gradle. Generated output is ignored. It stays outside the root `build/`
directory, which the existing desktop cross-build deletes.

## Preview and checks

```sh
npm run build:mobile
go -C mobile run ./cmd/preview --data /tmp/overland-mobile-preview
# Open the capability URL printed by the preview server.

npm run check:mobile
npm run test:mobile
go -C mobile test -race -cover ./host/...
go -C mobile vet ./host/... ./cmd/preview/...
```

`npm run dev:mobile` serves the UI on port 5174 and proxies API paths to the
standard desktop backend on port 8000. Filesystem draft storage is available in
the Android app and dedicated preview host, not the standard desktop server.
Native file dialogs are Android-only; browser preview uses browser files.

Startup serves the mobile UI and native draft/import endpoints immediately while
the backend verifies local caches, restores download pins and opens routing in
the background. Navigation and local GPX editing remain usable. A small notice
reports startup progress or failure; map tiles wait for the backend configuration
so startup cannot bypass offline policy through public-provider fallbacks.

Mobile browser tests use two phone sizes. They check file identity, create-only
imports, editing/undo, draft restoration, routing profiles and permits, unknown
elevation preservation, map-first layout, and combined area downloads.

For an attached debug build:

```sh
PID=$(adb shell pidof co.overland.mobile)
adb forward tcp:9222 localabstract:webview_devtools_remote_$PID
node mobile/e2e/device-smoke.mjs --prepare
node mobile/e2e/device-restart.mjs
```

The smoke script explicitly downloads **Monaco**, then verifies all four riding
profiles in cache-only mode. The restart script checks recovery of the existing
draft without editing it. Set `ANDROID_SERIAL` when multiple devices are attached.
Debug WebViews can also be inspected in Chrome at `chrome://inspect`.

## Architecture

The mobile Go module depends on the parent module through a local `replace`.
Wails stays out of the existing server/CLI module. `frontend/src` imports shared
logic from `src/lib` and reuses the map-rendering components; it does not import
the desktop App or its CSS.

Wails boots the Android process and native APIs. Its asset handler supplies a
bootstrap page that navigates the WebView to a loopback server on a random port.
That server mounts the existing Chi handler and mobile assets. A random
capability is exchanged for an HttpOnly, SameSite cookie; every resource/API
request requires it. The cookie stays in the WebView, and navigation is limited
to the app's specific origin. External HTTPS links open outside the app. This
preserves HTTP bodies, statuses, query parameters, cookies and binary map tiles
that Wails' Android asset transport does not carry fully.

`host` adds only native import/share/location endpoints and atomic draft storage.
GPX files, responses, elevation tiles and Broom data live under Android's private
files directory; downloaded data is not placed in the OS-evictable cache. The
map and elevation caches use available device storage rather than fixed GiB
budgets. Writes retain a 64 MiB free-space margin; both caches share the same
filesystem space. There is no fixed saved-pack count, and retries reuse an
incomplete pack's identity. Metadata is included in storage accounting. The
200,000-entry index bound and per-job/worker limits still bound memory and
provider traffic. Downloaded pack entries and
terrain tiles are pinned, including across restarts. Broom retains its
own independent managed cache. Android preparation uses one builder job and one
concurrent route query.

A Go watcher follows backend download jobs and manages Wails' data-sync
foreground service independently of WebView timers. Android can still terminate
an app or time-limit background work. Backend atomic writes and existing
preparation/cache reuse handle interruption; the area button can retry.

The native library is linked with 16 KiB segment alignment. Physical testing has
used a Pixel 10 Pro XL (Android 17, Vanadium 153, 4 KiB pages); a 16 KiB device
runtime has not been tested.
