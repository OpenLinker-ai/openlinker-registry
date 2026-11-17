#!/usr/bin/env bash
# Builds one release archive of openlinker-registry and its SHA-256 file.
#
#   scripts/package-release.sh <version> <commit> <goos> <goarch> <output dir>
#
# The archive is openlinker-registry-<version>-<goos>-<goarch>.tar.gz (.zip
# for Windows) holding one directory of that name with the binary, the
# READMEs and the license. Database migrations and the built-in pages are
# compiled into the binary. The Compose example under deploy/ builds from
# source, so it is not part of an archive.
set -euo pipefail

if [ "$#" -ne 5 ]; then
  echo "usage: $0 <version> <commit> <goos> <goarch> <output dir>" >&2
  exit 2
fi
version="$1" commit="$2" goos="$3" goarch="$4" out_dir="$5"

# These go into a file name and into -ldflags, so they are checked first.
[[ "${version}" =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || { echo "invalid version: ${version}" >&2; exit 2; }
[[ "${commit}" =~ ^[0-9a-f]{40}$ ]] || { echo "commit must be a full commit SHA: ${commit}" >&2; exit 2; }
[[ "${goos}" =~ ^[a-z0-9]+$ && "${goarch}" =~ ^[a-z0-9]+$ ]] || { echo "invalid platform: ${goos}/${goarch}" >&2; exit 2; }

root_dir="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "${out_dir}"
out_dir="$(cd "${out_dir}" && pwd)"

artifact_base="openlinker-registry-${version}-${goos}-${goarch}"
work_root="$(mktemp -d)"
trap 'rm -rf "${work_root}"' EXIT
work_dir="${work_root}/${artifact_base}"
mkdir -p "${work_dir}"

binary_name="openlinker-registry"
if [ "${goos}" = windows ]; then
  binary_name="${binary_name}.exe"
fi

(
  cd "${root_dir}"
  GOWORK=off CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${version} -X main.commit=${commit}" \
    -o "${work_dir}/${binary_name}" ./cmd/openlinker-registry
  cp README.md README.zh-CN.md LICENSE "${work_dir}/"
)

if [ "${goos}" = windows ]; then
  archive="${artifact_base}.zip"
  (cd "${work_root}" && zip -qr "${out_dir}/${archive}" "${artifact_base}")
else
  archive="${artifact_base}.tar.gz"
  tar -C "${work_root}" -czf "${out_dir}/${archive}" "${artifact_base}"
fi

(
  cd "${out_dir}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${archive}" > "${archive}.sha256"
  else
    shasum -a 256 "${archive}" > "${archive}.sha256"
  fi
)

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "artifact_base=${artifact_base}" >> "${GITHUB_OUTPUT}"
fi
echo "${out_dir}/${archive}"
