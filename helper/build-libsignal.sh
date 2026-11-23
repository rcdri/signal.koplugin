#!/bin/sh
# Builds libsignal-ffi and zlib for arm-linux-musleabihf into $CROSS/arm/lib.
# Run from this folder, with ../mautrix-signal checked out (git submodule update --init).
set -e
CROSS=${CROSS:-$(pwd)/cross}
LIBSIGNAL=../mautrix-signal/pkg/libsignalgo/libsignal
TARGET=armv7-unknown-linux-musleabihf
mkdir -p "$CROSS/arm/lib" "$CROSS/arm/include"

# zig wrappers used by cgo
cat > "$CROSS/zcc" <<'EOS'
#!/bin/sh
exec python3 -m ziglang cc -target arm-linux-musleabihf -mcpu=generic+v7a+vfp3d16+thumb2 "$@"
EOS
sed 's/ziglang cc/ziglang c++/' "$CROSS/zcc" > "$CROSS/zcxx"
chmod +x "$CROSS/zcc" "$CROSS/zcxx"

# signalmeow pre-allocates attachment files with fallocate, which FAT (the
# Kindle's /mnt/us) on kernels before 4.18 rejects; make it best-effort.
if ! grep -q "sig-helper patch" ../mautrix-signal/pkg/signalmeow/attachments.go; then
	git -C ../mautrix-signal apply "$(pwd)/patches/signalmeow-fat-fallocate.patch"
fi

# libsignal (Rust). boring passes a 32-bit time_t where musl 1.2 headers use 64-bit.
(cd "$LIBSIGNAL" && rustup toolchain install "$(cat rust-toolchain)" --profile minimal --target $TARGET && cargo fetch)
patched=0
for f in "${CARGO_HOME:-$HOME/.cargo}"/git/checkouts/boring-*/*/boring/src/x509/verify.rs; do
	[ -f "$f" ] || continue
	sed -i 's/X509_VERIFY_PARAM_set_time(self.as_ptr(), time)/X509_VERIFY_PARAM_set_time(self.as_ptr(), time as _)/' "$f"
	grep -q "time as _" "$f" && patched=1
done
[ "$patched" = 1 ] || { echo "boring's x509/verify.rs not found or not patched" >&2; exit 1; }
(cd "$LIBSIGNAL" && cargo zigbuild -p libsignal-ffi --release --target $TARGET)
cp "$LIBSIGNAL/target/$TARGET/release/libsignal_ffi.a" "$CROSS/arm/lib/"

# zlib
if [ ! -f "$CROSS/arm/lib/libz.a" ]; then
	rm -rf "$CROSS/zlib" && git clone -q --depth 1 -b v1.3.1 https://github.com/madler/zlib.git "$CROSS/zlib"
	mkdir -p "$CROSS/zobj"
	for f in adler32 crc32 deflate infback inffast inflate inftrees trees zutil compress uncompr gzclose gzlib gzread gzwrite; do
		"$CROSS/zcc" -O2 -D_LARGEFILE64_SOURCE=1 -DHAVE_HIDDEN -c "$CROSS/zlib/$f.c" -o "$CROSS/zobj/$f.o"
	done
	python3 -m ziglang ar rcs "$CROSS/arm/lib/libz.a" "$CROSS"/zobj/*.o
	cp "$CROSS/zlib/zlib.h" "$CROSS/zlib/zconf.h" "$CROSS/arm/include/"
fi
# libsignalgo links -lstdc++; zig provides libc++ instead
python3 -m ziglang ar rcs "$CROSS/arm/lib/libstdc++.a"
echo "Done. Now: CROSS=$CROSS ./build-arm.sh"
