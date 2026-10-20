#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
directory=${1:-"$root/../tap-sdk-loop-compat"}
source=${2:-https://github.com/lightninglabs/tap-sdk.git}
revision=932b4aa5429b66df2df13fcfac03fc6c6bdf974e
patch="$root/scripts/tap-sdk-compat.patch"

# Keep the normal SDK checkout and its upstream pull requests untouched.
if [ ! -e "$directory" ]; then
    git clone --no-checkout "$source" "$directory"
    git -C "$directory" checkout --detach "$revision"
fi
if [ "$(git -C "$directory" rev-parse HEAD)" != "$revision" ]; then
    echo "SDK checkout has another revision; preserve it before setup." >&2
    exit 1
fi
if git -C "$directory" apply --reverse --check "$patch" 2>/dev/null; then
    exit 0
fi
if [ -n "$(git -C "$directory" status --porcelain)" ]; then
    echo "SDK checkout has local changes; preserve them before setup." >&2
    exit 1
fi
git -C "$directory" apply --check "$patch"
git -C "$directory" apply "$patch"
