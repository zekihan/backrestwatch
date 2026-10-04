#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=$(./scripts/release_version.sh)
image="backrestwatch-smoke:$version"
docker build --build-arg "VERSION=$version" -t "$image" .
test "$(docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges "$image" --version)" = "$version"
test "$(docker image inspect --format '{{.Config.User}}' "$image")" = '25100:25100'
# Validate the real entrypoint and mounted configuration without API side effects.
docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  -v "$PWD/examples/config.json:/config/config.json:ro" "$image" --check-config
