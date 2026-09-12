#!/usr/bin/env bash

set -euo pipefail

: "${LAYER_CACHE_BIN:?set LAYER_CACHE_BIN to an installed Layer Cache binary}"
: "${QA_ROOT:?set QA_ROOT to an empty disposable directory}"
: "${BUILDX_PLATFORM:?set BUILDX_PLATFORM to linux/amd64 or linux/arm64}"

case "$BUILDX_PLATFORM" in
  linux/amd64 | linux/arm64) ;;
  *)
    echo "unsupported QA platform: $BUILDX_PLATFORM" >&2
    exit 2
    ;;
esac

QA_CONFIG="$QA_ROOT/config.json"
BUILDER="${BUILDER:-layercache-buildx-qa-$$}"
DOCKER_FIXTURE="$QA_ROOT/docker"
IMAGE="${IMAGE:-layercache-buildx-qa:$$}"
LAYER_CACHE_LISTEN="${LAYER_CACHE_LISTEN:-127.0.0.1:17437}"

cleanup() {
  "$LAYER_CACHE_BIN" stop --config "$QA_CONFIG" --json >/dev/null 2>&1 || true
  "$LAYER_CACHE_BIN" uninstall \
    --config "$QA_CONFIG" --delete-cache --yes --json >/dev/null 2>&1 || true
  docker buildx rm "$BUILDER" >/dev/null 2>&1 || true
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$QA_ROOT" "$DOCKER_FIXTURE"
docker info >/dev/null
docker buildx version

"$LAYER_CACHE_BIN" setup \
  --config "$QA_CONFIG" \
  --data-dir "$QA_ROOT/data" \
  --listen "$LAYER_CACHE_LISTEN" \
  --buildkit-builder "$BUILDER" \
  --max-size 1073741824 \
  --min-free-bytes 0 \
  --non-interactive --json >/dev/null
"$LAYER_CACHE_BIN" start --config "$QA_CONFIG" --json >/dev/null

printf 'hello from Layer Cache Buildx QA\n' >"$DOCKER_FIXTURE/payload.txt"
printf 'FROM scratch\nCOPY payload.txt /payload.txt\n' >"$DOCKER_FIXTURE/Dockerfile"
"$LAYER_CACHE_BIN" integration buildkit --config "$QA_CONFIG" --apply --json >/dev/null

"$LAYER_CACHE_BIN" buildx build --config "$QA_CONFIG" \
  --platform "$BUILDX_PLATFORM" --load --json -- \
  --tag "$IMAGE" --file "$DOCKER_FIXTURE/Dockerfile" "$DOCKER_FIXTURE" \
  >"$QA_ROOT/buildx-first.json"
FIRST_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE")"

"$LAYER_CACHE_BIN" buildx build --config "$QA_CONFIG" \
  --platform "$BUILDX_PLATFORM" --load --json -- \
  --tag "$IMAGE" --file "$DOCKER_FIXTURE/Dockerfile" "$DOCKER_FIXTURE" \
  >"$QA_ROOT/buildx-second.json"
SECOND_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$IMAGE")"

test "$FIRST_IMAGE_ID" = "$SECOND_IMAGE_ID"
jq -e '.metrics.cachedVertices > 0' "$QA_ROOT/buildx-second.json" >/dev/null
CONTAINER_ID="$(docker create "$IMAGE" /layercache-qa-not-executed)"
docker export "$CONTAINER_ID" | tar -xOf - payload.txt >"$QA_ROOT/buildx-payload.txt"
docker rm "$CONTAINER_ID" >/dev/null
test "$(cat "$QA_ROOT/buildx-payload.txt")" = "hello from Layer Cache Buildx QA"

printf 'Buildx cold/warm QA passed for %s with image %s.\n' "$BUILDX_PLATFORM" "$FIRST_IMAGE_ID"

cleanup
trap - EXIT
