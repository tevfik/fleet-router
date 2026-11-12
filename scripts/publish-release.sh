#!/usr/bin/env bash
# Publishes fleet to the Gitea server of the repository: the engine package (the full fleet-with-engine binaries, one per
# variant, xz-packed) and/or the release (tag + fleet-linux-* + SHA256SUMS).  Needs a token with write:package / write:repository
# in GITEA_TOKEN (the daemon's update token only reads).  A package version or tag that exists is never overwritten.
#
#   scripts/publish-release.sh engine <dir-with-fleet-engine-linux-*-files>      # version = EngineVersion in internal/update/engine.go
#   scripts/publish-release.sh release vX.Y.Z                                   # needs dist/ from `make release`
#
# The engine package is pinned by the fleet it is built into (EngineVersion): build every variant from the commit that is tagged
# (scripts/build-engine.sh with VERSION=vX.Y.Z, patches applied by the script), bump EngineVersion, publish the package FIRST,
# then the release.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
REPO=${REPO:-$(git -C "$ROOT" remote get-url origin | sed -E 's#^git@([^:]+):#https://\1/#; s#\.git$##')}
BASE=${REPO%/*/*}; OWNER=$(basename "$(dirname "$REPO")"); NAME=$(basename "$REPO")
# credentials: GITEA_TOKEN (write:package, write:repository), else the user name and password of the host in ~/.netrc
# the token goes to curl in a header file (-H @file, mode 600), never on its command line where `ps` shows it
if [ -n "${GITEA_TOKEN:-}" ]; then
    hdr=$(mktemp); chmod 600 "$hdr"; trap 'rm -f "$hdr"' EXIT
    printf 'Authorization: token %s\n' "$GITEA_TOKEN" > "$hdr"
    auth=(-H "@$hdr")
elif grep -q "machine $(h=${BASE#*://}; echo "${h%%:*}")" "$HOME/.netrc" 2>/dev/null; then auth=(--netrc)
else echo "need GITEA_TOKEN or a '$BASE' entry in ~/.netrc (machine HOST login USER password PASS, chmod 600)" >&2; exit 2; fi
case ${1:-} in
engine)
    dir=${2:?directory with the engine builds}
    ver=$(sed -n 's/^const EngineVersion = "\(.*\)"/\1/p' "$ROOT/internal/update/engine.go")
    [ -n "$ver" ] || { echo "EngineVersion not found" >&2; exit 2; }
    for f in "$dir"/fleet-engine-linux-*; do
        case $f in *.xz|*.sha256) continue ;; esac
        [ -f "$f" ] || continue
        if strings "$f" 2>/dev/null | grep -q '"cuda_archs":"native"'; then
            echo "refusing $f: built with ENGINE_CUDA_ARCHS=native (release packages require explicit arch list)" >&2
            exit 2
        fi
        n=$(basename "$f").xz
        [ -f "$f.xz" ] || xz -T0 -6 -k "$f"
        code=$(curl -s -o /dev/null -w '%{http_code}' "${auth[@]}" -X PUT --upload-file "$f.xz" "$BASE/api/packages/$OWNER/generic/fleet-engine/$ver/$n")
        echo "engine $ver $n: HTTP $code"
        case $code in 201) ;; 409) echo "  already published, not overwritten" ;; *) exit 1 ;; esac
    done ;;
release)
    tag=${2:?tag, e.g. v0.11.0}
    [ -f "$ROOT/dist/SHA256SUMS" ] || { echo "run make release first" >&2; exit 2; }
    id=$(curl -sf "${auth[@]}" -H 'Content-Type: application/json' -X POST "$BASE/api/v1/repos/$OWNER/$NAME/releases" \
        -d "{\"tag_name\":\"$tag\",\"target_commitish\":\"$(git -C "$ROOT" rev-parse HEAD)\",\"name\":\"$tag\",\"body\":\"${RELEASE_NOTES:-}\"}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
    for f in "$ROOT"/dist/*; do
        code=$(curl -s -o /dev/null -w '%{http_code}' "${auth[@]}" -X POST -F "attachment=@$f" "$BASE/api/v1/repos/$OWNER/$NAME/releases/$id/assets?name=$(basename "$f")")
        echo "asset $(basename "$f"): HTTP $code"
    done ;;
*) echo "usage: $0 engine <dir> | release vX.Y.Z" >&2; exit 2 ;;
esac
