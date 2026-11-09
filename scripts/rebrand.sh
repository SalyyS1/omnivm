#!/usr/bin/env bash
# rebrand.sh — rename the AegisVM code base to OmniVM.
#
# OmniVM is a fork of AegisVM (https://github.com/xfeldman/aegisvm). This
# script performs the mechanical rename in one pass so the same rename can be
# re-applied to patches cherry-picked from upstream:
#
#   git cherry-pick -n <upstream-commit> && scripts/rebrand.sh && git commit
#
# It is idempotent: running it on an already-renamed tree changes nothing.
# LICENSE, NOTICE, README.md and this script are never rewritten, because they
# carry the upstream attribution required by the Apache License 2.0.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Files to rewrite: every tracked text file except the attribution carriers.
list_files() {
    git ls-files -z "$@" \
        | grep -zv -e '^LICENSE$' -e '^NOTICE$' -e '^README.md$' -e '^scripts/rebrand.sh$' \
        | xargs -0 grep -lIZ -i 'aegis' 2>/dev/null || true
}

# 1. Python SDK: the import package becomes `omnivm` (not the CLI name).
if [ -d sdk/python/aegis ]; then
    git mv sdk/python/aegis sdk/python/omnivm
fi
list_files sdk/python | xargs -0 -r sed -i \
    -e 's/aegis-sdk/omnivm-sdk/g' \
    -e 's/\baegis\b/omnivm/g'

# 2. Directory renames for binaries and packaging.
for pair in \
    "cmd/aegis-mcp-guest:cmd/ovm-mcp-guest" \
    "cmd/aegis-mcp:cmd/ovm-mcp" \
    "cmd/aegis-harness:cmd/ovm-harness" \
    "cmd/aegisd:cmd/ovmd" \
    "cmd/aegis:cmd/ovm" \
    "debian/aegisvm:debian/omnivm"; do
    from="${pair%%:*}"
    to="${pair##*:}"
    if [ -d "$from" ]; then
        git mv "$from" "$to"
    fi
done

# 3. Content rewrite, ordered from most to least specific so that longer
#    tokens are not clobbered by the generic `aegis` rule at the end.
list_files | xargs -0 -r sed -i \
    -e 's#github\.com/xfeldman/aegisvm#github.com/SalyyS1/omnivm#g' \
    -e 's#xfeldman/aegisvm#SalyyS1/omnivm#g' \
    -e 's/X-Aegis-Instance/X-OmniVM-Instance/g' \
    -e 's/AEGIS_/OVM_/g' \
    -e 's/AegisVM/OmniVM/g' \
    -e 's/aegisvm/omnivm/g' \
    -e 's/aegis-mcp-guest/ovm-mcp-guest/g' \
    -e 's/aegis-mcp/ovm-mcp/g' \
    -e 's/aegis-harness/ovm-harness/g' \
    -e 's/aegisd/ovmd/g' \
    -e 's#\.aegis\b#.omnivm#g' \
    -e 's/Aegis/OmniVM/g' \
    -e 's/aegis/ovm/g'

echo "rebrand: done"
