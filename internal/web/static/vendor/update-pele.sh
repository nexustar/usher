#!/usr/bin/env bash
#
# Fetch pele from npm, then build from the GitHub tag and verify the two
# match byte-for-byte.  On success the npm copy is placed into the vendor
# directory; on mismatch the script aborts with a non-zero exit.
#
# Usage:
#   internal/web/static/vendor/update-pele.sh TAG
#
# Examples: 1.6.0

set -euo pipefail

VENDOR_DIR="$(cd "$(dirname "$0")" && pwd)"
VENDOR_FILE="${VENDOR_DIR}/pele.js"

TAG="${1:?usage: $0 <tag>  (e.g. 1.6.0)}"

echo "==> pele ${TAG}"

# --- fetch from npm ----------------------------------------------------------

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

NPM_FILE="${WORK}/npm.js"
NPM_URL="https://unpkg.com/pele@${TAG}/dist/pele.js"

echo "--- fetching npm artifact from ${NPM_URL}"
curl -fsSL "$NPM_URL" -o "$NPM_FILE"

NPM_SHA=$(sha256sum "$NPM_FILE" | cut -d' ' -f1)
echo "    sha256: ${NPM_SHA}  ($(wc -c < "$NPM_FILE") bytes)"

# --- build from source -------------------------------------------------------

SRC_DIR="${WORK}/src"
echo "--- cloning obsidianmd/pele @ ${TAG}"
git clone --quiet --depth 1 --branch "${TAG}" \
    https://github.com/obsidianmd/pele.git "$SRC_DIR"

echo "--- installing build deps"
(cd "$SRC_DIR" && npm ci --ignore-scripts --silent 2>/dev/null \
  && npm approve-scripts esbuild 2>/dev/null) >/dev/null || true

echo "--- building"
(cd "$SRC_DIR" && node scripts/build.mjs) >/dev/null

BUILD_FILE="${SRC_DIR}/dist/pele.js"
BUILD_SHA=$(sha256sum "$BUILD_FILE" | cut -d' ' -f1)
echo "    sha256: ${BUILD_SHA}  ($(wc -c < "$BUILD_FILE") bytes)"

# --- compare ------------------------------------------------------------------

if [[ "$NPM_SHA" != "$BUILD_SHA" ]]; then
  echo
  echo "FAIL: npm artifact does not match source build" >&2
  echo "  npm:   ${NPM_SHA}" >&2
  echo "  build: ${BUILD_SHA}" >&2
  exit 1
fi

echo
echo "OK: npm artifact matches source build"

# --- install ------------------------------------------------------------------

cp "$NPM_FILE" "$VENDOR_FILE"
curl -fsSL "https://unpkg.com/pele@${TAG}/LICENSE" -o "${VENDOR_DIR}/pele.LICENSE"
echo "==> vendored ${VENDOR_FILE} (${TAG}, sha256:${NPM_SHA})"
