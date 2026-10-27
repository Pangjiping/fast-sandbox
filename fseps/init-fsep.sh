#!/usr/bin/env bash

# Copyright 2025 The OpenSandbox Authors
# Copyright 2026 The Fast Sandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Adapted from OpenSandbox's oseps/init-osep.sh.
set -euo pipefail
# Bash 5.2+ otherwise treats '&' in replacement values as the matched token.
shopt -u patsub_replacement 2>/dev/null || true

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="$SCRIPT_DIR/fsep-template.md.template"
VALID_STATUSES="draft accepted implemented closed"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS] <title>

Create a Fast Sandbox Enhancement Proposal in fseps/.

Options:
  -a, --author AUTHOR   Author (default: Git user.name, user.email, or USER)
  -s, --status STATUS   Initial status (default: draft)
                        Valid: $VALID_STATUSES
      --slug SLUG       English filename slug (required for non-ASCII titles)
  -h, --help            Show this help message

Examples:
  $(basename "$0") "Snapshot Distribution"
  $(basename "$0") --author "@user" --slug snapshot-distribution "快照分发改进"

The script creates NNNN-slug.md using the next available proposal number.
Add the proposal to fseps/README.md before submitting a PR.
EOF
}

fail() {
    printf 'Error: %s\n' "$*" >&2
    exit 1
}

require_value() {
    [[ $# -ge 2 && -n "$2" && "$2" != -* ]] || fail "Missing value for $1"
}

yaml_escape() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    value="${value//$'\t'/\\t}"
    printf '%s' "$value"
}

TITLE=""
AUTHOR=""
STATUS="draft"
SLUG=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help)
            usage
            exit 0
            ;;
        -a|--author)
            require_value "$@"
            AUTHOR="$2"
            shift 2
            ;;
        -s|--status)
            require_value "$@"
            STATUS="$2"
            shift 2
            ;;
        --slug)
            require_value "$@"
            SLUG="$2"
            shift 2
            ;;
        -*)
            fail "Unknown option $1 (see --help)"
            ;;
        *)
            [[ -z "$TITLE" ]] || fail "Unexpected argument: $1"
            TITLE="$1"
            shift
            ;;
    esac
done

[[ "$TITLE" =~ [^[:space:]] ]] || fail "A proposal title is required (see --help)"
STATUS_VALID=false
for valid in $VALID_STATUSES; do
    if [[ "$STATUS" == "$valid" ]]; then
        STATUS_VALID=true
        break
    fi
done
[[ "$STATUS_VALID" == true ]] || fail "Invalid status '$STATUS'. Valid statuses: $VALID_STATUSES"

if [[ -z "$AUTHOR" ]]; then
    AUTHOR=$(git -C "$SCRIPT_DIR" config user.name 2>/dev/null || true)
    [[ -n "$AUTHOR" ]] || AUTHOR=$(git -C "$SCRIPT_DIR" config user.email 2>/dev/null || true)
    AUTHOR="${AUTHOR:-${USER:-Unknown Author}}"
fi
case "$TITLE$AUTHOR" in
    *$'\n'*|*$'\r'*) fail "Title and author must each be a single line" ;;
esac

if [[ -z "$SLUG" ]]; then
    # Non-ASCII titles require an explicit English slug rather than losing words.
    [[ $(LC_ALL=C printf '%s' "$TITLE" | LC_ALL=C tr -d '\000-\177') == "" ]] \
        || fail "Use --slug with an English filename slug for non-ASCII titles"
    SLUG=$(printf '%s' "$TITLE" | LC_ALL=C tr '[:upper:]' '[:lower:]' \
        | LC_ALL=C sed -E 's/[^a-z0-9 _-]//g; s/[ _-]+/-/g; s/^-+|-+$//g')
fi
[[ "$SLUG" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]] || fail "Slug must contain lowercase ASCII words separated by hyphens"
[[ -f "$TEMPLATE" ]] || fail "FSEP template not found at $TEMPLATE"

HIGHEST=0
for file in "$SCRIPT_DIR"/[0-9][0-9][0-9][0-9]-*.md; do
    [[ -f "$file" ]] || continue
    name="${file##*/}"
    number=$((10#${name%%-*}))
    if (( number > HIGHEST )); then
        HIGHEST=$number
    fi
done
(( HIGHEST < 9999 )) || fail "Four-digit proposal numbers are exhausted"
PROPOSAL_ID=$(printf '%04d' "$((HIGHEST + 1))")
DESTINATION="$SCRIPT_DIR/$PROPOSAL_ID-$SLUG.md"
DATE=$(date +%Y-%m-%d)
TITLE_YAML=$(yaml_escape "$TITLE")
AUTHOR_YAML=$(yaml_escape "$AUTHOR")

content=$(<"$TEMPLATE")
content="${content//\{\{proposal_id\}\}/$PROPOSAL_ID}"
content="${content//\{\{date\}\}/$DATE}"
content="${content//\{\{status_metadata\}\}/$STATUS}"
content="${content//\{\{title\}\}/$TITLE_YAML}"
content="${content//\{\{author\}\}/$AUTHOR_YAML}"
content="${content//\{\{heading\}\}/$TITLE}"

# Never overwrite an existing proposal, including during concurrent invocations.
(set -o noclobber; printf '%s\n' "$content" > "$DESTINATION") \
    || fail "Could not create $DESTINATION; check existing files and retry"
printf 'Created %s\n' "$DESTINATION"
printf 'Next: complete the proposal and add it to fseps/README.md.\n'
