#!/bin/sh
# Linux native tool build. Dependencies: C/C++ toolchain, cmake, libaio/curl/
# ext2fs/gflags/glib2/nl/ssl/zstd/uuid/zlib development packages, Go and Git.
set -eu
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
out="$repo_root/.build/oci-tools"
mkdir -p "$out/bin" "$out/lib" "$repo_root/.build/linux-amd64"
git -C "$work" clone https://github.com/containerd/overlaybd.git
cd "$work/overlaybd"
git checkout --detach 5119193d540010e09c0af98add1ef2b0f4a51bc0
git apply "$repo_root/build/oci/overlaybd-identitytoken.patch"
# The checkpoint packer imports captured raw block roots before compression.
install -m 0644 "$repo_root/build/sandboxtemplate-builder/overlaybd-import-raw.cpp" src/tools/overlaybd-import-raw.cpp
sed -i '/add_executable(overlaybd-create/i add_executable(overlaybd-import-raw overlaybd-import-raw.cpp)\ntarget_include_directories(overlaybd-import-raw PUBLIC ${PHOTON_INCLUDE_DIR})\ntarget_link_libraries(overlaybd-import-raw photon_static overlaybd_lib)\n' src/tools/CMakeLists.txt
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_TESTING=OFF -DBUILD_STREAM_CONVERTOR=OFF -DBUILD_UBLK_FRONTEND=ON -DORIGIN_EXT2FS=OFF
cmake --build build --target overlaybd-import-raw overlaybd-ublkd overlaybd-create overlaybd-apply overlaybd-commit overlaybd-resize --parallel "${JOBS:-8}"
for tool in overlaybd-import-raw overlaybd-ublkd overlaybd-create overlaybd-apply overlaybd-commit overlaybd-resize; do cp "build/output/$tool" "$out/bin/"; done
# resize uses OverlayBD's patched ext2fs library, not the system copy.
find build -name 'libext2fs.so*' -type f -exec cp '{}' "$out/lib/" \;
if [ ! -e "$out/lib/libext2fs.so.2" ]; then
  library=$(find "$out/lib" -name 'libext2fs.so*' -type f | head -1)
  test -n "$library"
  cp "$library" "$out/lib/libext2fs.so.2"
fi
git -C "$work" clone https://github.com/containerd/accelerated-container-image.git
cd "$work/accelerated-container-image"
git checkout --detach a00e366f9bfc970ec4aaa00887541c4c1d9f9dab
CGO_ENABLED=0 go build -o "$repo_root/.build/linux-amd64/convertor" ./cmd/convertor
