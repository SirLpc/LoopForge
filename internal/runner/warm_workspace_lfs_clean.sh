#!/bin/sh
# Git LFS clean passes through valid pointers shorter than 1024 bytes,
# including noncanonical encodings. Preserve those exact bytes.
set -eu
prefix=$(mktemp "${TMPDIR:-/tmp}/loopforge-lfs-prefix.XXXXXXXX")
trap 'rm -f "$prefix"' 0
trap 'exit 1' HUP INT TERM
head -c 1024 > "$prefix"
size=$(wc -c < "$prefix")
if [ "$size" -lt 1024 ]; then
    if git-lfs pointer --check --no-strict --stdin < "$prefix"; then
        cat "$prefix"
        exit 0
    else
        status=$?
        [ "$status" -eq 1 ] || exit "$status"
    fi
fi

# Hash the prefix and remaining stream without populating lfs.storage.
# Successful removal also confirms the pipeline's writer reached EOF.
(cat "$prefix" - && rm "$prefix") | git-lfs pointer --file=/dev/stdin 2>/dev/null || {
    echo "git-lfs could not generate a verification pointer" >&2
    exit 1
}
[ ! -e "$prefix" ]
