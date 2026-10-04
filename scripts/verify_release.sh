#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=${1:?release version required}
test "$version" = "$(./scripts/release_version.sh)"
mkdir -p dist/verified
# Run with fresh credentials stores to verify public access, even on an authenticated workstation.
verify_dir=$(mktemp -d)
trap 'rm -rf "$verify_dir"' EXIT HUP INT TERM
for image in docker.io/zekihan/backrestwatch ghcr.io/zekihan/backrestwatch; do
  DOCKER_CONFIG="$verify_dir/docker" docker buildx imagetools inspect "$image:$version" --raw > "$verify_dir/manifest.json"
  python3 - "$verify_dir/manifest.json" <<'PY'
import json, sys
manifest = json.load(open(sys.argv[1]))
platforms = {(m.get('platform', {}).get('os'), m.get('platform', {}).get('architecture')) for m in manifest['manifests']}
assert {('linux', 'amd64'), ('linux', 'arm64')} <= platforms, platforms
PY
  DOCKER_CONFIG="$verify_dir/docker" docker buildx imagetools inspect "$image:$version"
done
HELM_REGISTRY_CONFIG="$verify_dir/registry.json" helm pull oci://ghcr.io/zekihan/charts/backrestwatch --version "$version" --destination dist/verified
helm show chart "dist/verified/backrestwatch-$version.tgz" > "$verify_dir/chart.yaml"
test "$(sed -n 's/^version: //p' "$verify_dir/chart.yaml")" = "$version"
# Helm normalizes appVersion quoting when displaying metadata.
app_version=$(sed -n 's/^appVersion: //p' "$verify_dir/chart.yaml" | tr -d '"')
test "$app_version" = "$version"
helm template verify "dist/verified/backrestwatch-$version.tgz" -n backup-monitoring > "$verify_dir/rendered.yaml"
test -s "$verify_dir/rendered.yaml"
printf 'Verified both image platforms and OCI chart %s\n' "$version"
