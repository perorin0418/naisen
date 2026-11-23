#!/usr/bin/env bash
# リリース用のバイナリを dist/ に作る。
# 使い方: scripts/build-release.sh v0.3.0
set -euo pipefail

version=${1:?使い方: $0 <バージョン（例: v0.3.0）>}
cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist

targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64)
for t in "${targets[@]}"; do
  os=${t%/*} arch=${t#*/}
  name="naisen_${version#v}_${os}_${arch}"
  bin=naisen
  [ "$os" = windows ] && bin=naisen.exe
  mkdir -p "dist/$name"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "dist/$name/$bin" ./cmd/naisen
  cp README.md "dist/$name/"
  if [ "$os" = windows ]; then
    (cd dist && python3 -m zipfile -c "$name.zip" "$name")
  else
    tar -C dist -czf "dist/$name.tar.gz" "$name"
  fi
  rm -r "dist/$name"
done
(cd dist && sha256sum naisen_* > checksums.txt)
ls -l dist
