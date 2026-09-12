#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 3 ]]; then
  echo "usage: qa/install-buildx-client.sh VERSION SHA256 DOCKER_CONFIG" >&2
  exit 2
fi

VERSION="$1"
EXPECTED_SHA256="$2"
DOCKER_CONFIG_DIR="$3"

if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Buildx version must be an exact stable semantic version." >&2
  exit 2
fi
if [[ ! "$EXPECTED_SHA256" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Buildx SHA-256 must be exactly 64 lowercase hexadecimal characters." >&2
  exit 2
fi

case "$(uname -m)" in
  x86_64) ASSET_ARCH="amd64" ;;
  aarch64 | arm64) ASSET_ARCH="arm64" ;;
  *)
    echo "unsupported Buildx QA architecture: $(uname -m)" >&2
    exit 2
    ;;
esac

PLUGIN_DIR="$DOCKER_CONFIG_DIR/cli-plugins"
PLUGIN_PATH="$PLUGIN_DIR/docker-buildx"
DOWNLOAD_PATH="$PLUGIN_PATH.download"
ASSET="buildx-v$VERSION.linux-$ASSET_ARCH"
URL="https://github.com/docker/buildx/releases/download/v$VERSION/$ASSET"

mkdir -p "$PLUGIN_DIR"
rm -f "$DOWNLOAD_PATH"
trap 'rm -f "$DOWNLOAD_PATH"' EXIT
curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
  --output "$DOWNLOAD_PATH" "$URL"
ACTUAL_SHA256="$(sha256sum "$DOWNLOAD_PATH" | awk '{print $1}')"
if [[ "$ACTUAL_SHA256" != "$EXPECTED_SHA256" ]]; then
  echo "Buildx download failed SHA-256 verification." >&2
  exit 1
fi
chmod 0755 "$DOWNLOAD_PATH"
mv "$DOWNLOAD_PATH" "$PLUGIN_PATH"
trap - EXIT

DOCKER_CONFIG="$DOCKER_CONFIG_DIR" docker buildx version
