# OverlandX

OverlandX runs the complete Overland GPX editor in a native desktop window. It
uses [Glaze](https://github.com/crgimenes/glaze), so it opens no external browser
and does not bundle Chromium. The frontend, Overland API, and desktop host are
linked into one binary.

## Build

Go 1.27 and Node.js 22 or newer are required to build from the repository root:

```bash
make -C overlandx
./overlandx/overlandx
```

The build runs Overland's frontend build first, then compiles it into the
OverlandX executable. The output is the single file `overlandx/overlandx`.

Set a release version with `make -C overlandx VERSION=v0.1.0`; local builds
report `dev`.

Glaze uses the operating system WebView at runtime:

- Linux: WebKitGTK 4.1 or 6.0
- macOS: WKWebView, included with macOS
- Windows: WebView2, included with current Windows 10 and 11

For example, Fedora installs the Linux runtime with `dnf install webkit2gtk4.1`;
Debian and Ubuntu use `apt install libwebkit2gtk-4.1-0`.

## Usage

```bash
./overlandx/overlandx -help
./overlandx/overlandx -gpx-dir ./tracks
./overlandx/overlandx -debug
```

Tracks and elevation tiles use Overland's existing XDG defaults. The WebView is
served on a random loopback-only port for the lifetime of the window. Closing
the window stops the server and releases the track library.

## Layout

- `main.go` hosts Overland with Glaze.
- `../overland.go` exposes the shared application as an `http.Handler`.
- `go.mod` keeps Glaze and its Go 1.27 toolchain separate from the server module.

The local `replace` in `go.mod` points at the parent checkout. The frontend is
built from that checkout before Go embeds it, so OverlandX has no vendored copy
of Overland and cannot drift from the browser application.

Run `make -C overlandx check` for the desktop host checks. Run the repository's
top-level `make check` for the shared frontend and backend checks.
