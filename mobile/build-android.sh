#!/usr/bin/env bash
set -euo pipefail
ROOT="$(dirname "$(realpath "$0")")"
WAILS_VERSION=v3.0.0-beta.25
SDK="${ANDROID_HOME:-$HOME/Android/Sdk}"
NDK="${ANDROID_NDK_HOME:-$SDK/ndk/27.2.12479018}"
if [[ ! -d "$NDK" && -z "${ANDROID_NDK_HOME:-}" ]]; then NDK="$SDK/ndk/android-ndk-r27c"; fi
if [[ ! -x "$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android26-clang" ]]; then
  printf 'Install Android NDK r27c or set ANDROID_NDK_HOME to its directory.\n' >&2
  exit 1
fi
ARCH=arm64-v8a
MODE="${1:-debug}"
case "$MODE" in debug|release) ;; *) printf 'Usage: %s [debug|release]\n' "$0"; exit 1;; esac
mkdir -p "$ROOT/bin/tools" "$ROOT/build"
if [[ ! -x "$ROOT/bin/tools/wails3" ]]; then
  # The host CLI only generates build assets; it needs no GTK/WebKit linkage.
  # CGO is enabled separately below for the Android shared library.
  CGO_ENABLED=0 GOBIN="$ROOT/bin/tools" go install "github.com/wailsapp/wails/v3/cmd/wails3@$WAILS_VERSION"
fi
"$ROOT/bin/tools/wails3" generate build-assets -dir "$ROOT/build" -name overland-mobile -binaryname overland-mobile -productname Overland -productidentifier co.overland.mobile -silent
python3 "$ROOT/android/configure.py"
npm --prefix "$ROOT/.." run build:mobile
mkdir -p "$ROOT/build/android/app/src/main/jniLibs/$ARCH"
export ANDROID_HOME="$SDK"
export CC="$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android26-clang"
export CGO_ENABLED=1 GOOS=android GOARCH=arm64
export CGO_LDFLAGS="-Wl,-z,max-page-size=16384"
TAGS=android,debug
[[ "$MODE" == release ]] && TAGS=android,production
go -C "$ROOT" build -buildmode=c-shared -tags "$TAGS" -trimpath -ldflags='-s -w' -o "$ROOT/build/android/app/src/main/jniLibs/$ARCH/libwails.so" ./cmd/overland
unset GOOS GOARCH CGO_ENABLED CC CGO_LDFLAGS
bash "$ROOT/build/android/gradlew" -p "$ROOT/build/android" "assemble${MODE^}" --console=plain
cp "$ROOT/build/android/app/build/outputs/apk/$MODE/app-$MODE.apk" "$ROOT/bin/overland-$MODE.apk"
printf '\nAPK: %s/bin/overland-%s.apk\nInstall without deleting data: adb install -r %s/bin/overland-%s.apk\n' "$ROOT" "$MODE" "$ROOT" "$MODE"
