#!/bin/sh
# Cross-compiles sig-helper for 32-bit ARM Kindles (armhf, static musl).
# Needs: zig (pip install ziglang), libsignal_ffi.a, libz.a and an empty
# libstdc++.a for arm-linux-musleabihf in $CROSS/lib (see README.md).
set -e
CROSS=${CROSS:-$(pwd)/cross}
TAGS=${TAGS:-}
OUT=${OUT:-sig-helper-armhf}
VERSION=${VERSION:-dev}
export CGO_ENABLED=1 GOOS=linux GOARCH=arm GOARM=7
export CC="$CROSS/zcc" CXX="$CROSS/zcxx"
export CGO_CFLAGS="-O2 -I$CROSS/arm/include"
export CGO_LDFLAGS="-L$CROSS/arm/lib -lc++ -lunwind"
go build -trimpath -tags "netgo osusergo sqlite_omit_load_extension $TAGS" -ldflags "-s -w -X main.version=$VERSION -linkmode external -extldflags -static" -o "$OUT" .
