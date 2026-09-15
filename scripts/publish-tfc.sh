#!/usr/bin/env bash
# Publish one provider version to an HCP Terraform private registry — local and CI.
#   scripts/publish-tfc.sh <org> <name> <version> <dist-dir>
# env: TFC_TOKEN (required) · GPG_PUBKEY_FILE (ASCII-armored public key; needed once per key)
#      PLATFORMS (default "linux_amd64 darwin_arm64")
set -euo pipefail
ORG="${1:?org}"; NAME="${2:?name}"; VER="${3:?version}"; DIST="${4:?dist dir}"
: "${TFC_TOKEN:?TFC_TOKEN is required}"
API=https://app.terraform.io/api/v2
api() { curl -sS --fail-with-body -H "Authorization: Bearer $TFC_TOKEN" -H "Content-Type: application/vnd.api+json" "$@"; }
jsonq() { python3 -c "import sys,json; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }

BIN="terraform-provider-${NAME}"
SUMS="$DIST/${BIN}_${VER}_SHA256SUMS"; SIG="$SUMS.sig"
[[ -f "$SUMS" && -f "$SIG" ]] || { echo "missing: $SUMS / $SIG" >&2; exit 1; }

# 1) provider (idempotent)
api -X POST "$API/organizations/$ORG/registry-providers" \
  -d "{\"data\":{\"type\":\"registry-providers\",\"attributes\":{\"name\":\"$NAME\",\"namespace\":\"$ORG\",\"registry-name\":\"private\"}}}" >/dev/null 2>&1 || true

# 2) GPG key: the long key id that signed SHA256SUMS; register the public key if unknown
KEYID=$(gpg --list-packets "$SIG" 2>/dev/null | awk '/keyid/{print $NF; exit}')
HAVE=$(api "https://app.terraform.io/api/registry/private/v2/gpg-keys?filter%5Bnamespace%5D=$ORG" | jsonq "','.join(k['attributes']['key-id'] for k in d['data'])")
if [[ ",$HAVE," != *",$KEYID,"* ]]; then
  : "${GPG_PUBKEY_FILE:?key $KEYID is not registered for $ORG; set GPG_PUBKEY_FILE}"
  ARMOR=$(python3 -c "import json,sys; print(json.dumps(open(sys.argv[1]).read()))" "$GPG_PUBKEY_FILE")
  api -X POST "https://app.terraform.io/api/registry/private/v2/gpg-keys" \
    -d "{\"data\":{\"type\":\"gpg-keys\",\"attributes\":{\"namespace\":\"$ORG\",\"ascii-armor\":$ARMOR}}}" | jsonq "'registered gpg key '+d['data']['attributes']['key-id']"
fi

# 3) version → upload checksums and signature
VJSON=$(api -X POST "$API/organizations/$ORG/registry-providers/private/$ORG/$NAME/versions" \
  -d "{\"data\":{\"type\":\"registry-provider-versions\",\"attributes\":{\"version\":\"$VER\",\"key-id\":\"$KEYID\",\"protocols\":[\"6.0\"]}}}")
curl -sS -T "$SUMS" "$(echo "$VJSON" | jsonq "d['data']['links']['shasums-upload']")"
curl -sS -T "$SIG"  "$(echo "$VJSON" | jsonq "d['data']['links']['shasums-sig-upload']")"

# 4) platforms → upload binaries
for P in ${PLATFORMS:-linux_amd64 darwin_arm64}; do
  OS="${P%_*}"; ARCH="${P#*_}"; ZIP="$DIST/${BIN}_${VER}_${OS}_${ARCH}.zip"
  [[ -f "$ZIP" ]] || { echo "skip $P (no zip)"; continue; }
  SHA=$(shasum -a 256 "$ZIP" | awk '{print $1}')
  PJSON=$(api -X POST "$API/organizations/$ORG/registry-providers/private/$ORG/$NAME/versions/$VER/platforms" \
    -d "{\"data\":{\"type\":\"registry-provider-platforms\",\"attributes\":{\"os\":\"$OS\",\"arch\":\"$ARCH\",\"shasum\":\"$SHA\",\"filename\":\"$(basename "$ZIP")\"}}}")
  curl -sS -T "$ZIP" "$(echo "$PJSON" | jsonq "d['data']['links']['provider-binary-upload']")"
  echo "uploaded $P"
done
echo "published $ORG/$NAME $VER"
