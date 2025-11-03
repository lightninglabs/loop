#!/bin/bash

# Simple bash script to build basic loop tools for all the platforms
# we support with the golang cross-compiler.
#
# Copyright (c) 2016 Company 0, LLC.
# Use of this source code is governed by the ISC
# license.

# Exit on errors.
set -e

# Get the directory of the script
SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

# Checkout the repo to a subdir to clean from clean from unstaged files and
# build exactly what is committed.
BUILD_DIR="${SCRIPT_DIR}/tmp-build-$(date +%Y%m%d-%H%M%S)"

# green prints one line of green text (if the terminal supports it).
function green() {
  printf "\e[0;32m%s\e[0m\n" "${1}"
}

# red prints one line of red text (if the terminal supports it).
function red() {
  printf "\e[0;31m%s\e[0m\n" "${1}"
}

# Use GO_CMD from env if set, otherwise default to "go".
GO_CMD="${GO_CMD:-go}"

# Check if the command exists.
if ! command -v "$GO_CMD" >/dev/null 2>&1; then
    red "Error: Go command '$GO_CMD' not found"
    exit 1
fi

# Make sure we have the expected Go version installed.
EXPECTED_VERSION="go1.24.6"
INSTALLED_VERSION=$("$GO_CMD" version 2>/dev/null | awk '{print $3}')
if [ "$INSTALLED_VERSION" = "$EXPECTED_VERSION" ]; then
    green "Go version matches expected: $INSTALLED_VERSION"
else
    red "Error: Expected Go version $EXPECTED_VERSION but found $INSTALLED_VERSION"
    exit 1
fi

TAG=''

check_tag() {
    # If no tag specified, use date + version otherwise use tag.
    if [[ $1x = x ]]; then
        TAG=`date +%Y%m%d-%H%M%S`
        green "No tag specified, using ${TAG} as tag"

        return
    fi

    TAG=$1

    # If a tag is specified, ensure that tag is present and checked out.
    if [[ $TAG != $(git describe --abbrev=10) ]]; then
        red "tag $TAG not checked out"
        exit 1
    fi

    # Verify that it is signed if it is a real tag. If the tag looks like the
    # output of "git describe" for an untagged commit, skip verification.
    # The pattern is: <tag_name>-<number_of_commits>-g<abbreviated_commit_hash>
    # Example: "v0.31.2-beta-122-g8c6b73c".
    if [[ $TAG =~ -[0-9]+-g([0-9a-f]{10})$ ]]; then
        # This looks like a "git describe" output. Make sure the hash
        # described is a prefix of the current commit.
        DESCRIBED_HASH=${BASH_REMATCH[1]}
        CURRENT_HASH=$(git rev-parse HEAD)
        if [[ $CURRENT_HASH != $DESCRIBED_HASH* ]]; then
            red "Described hash $DESCRIBED_HASH is not a prefix of current commit $CURRENT_HASH"
            exit 1
        fi

        return
    fi

    if ! git verify-tag $TAG; then
        red "tag $TAG not signed"
        exit 1
    fi

    # Build loop to extract version.
    make

    # Extract version command output.
    LOOP_VERSION_OUTPUT=`./loopd-debug --version`

    # Use a regex to isolate the version string.
    LOOP_VERSION_REGEX="version ([^ ]+) "
    if [[ $LOOP_VERSION_OUTPUT =~ $LOOP_VERSION_REGEX ]]; then
        # Prepend 'v' to match git tag naming scheme.
        LOOP_VERSION="v${BASH_REMATCH[1]}"
        echo "version: $LOOP_VERSION"

        # Match git tag with loop version.
        if [[ $TAG != $LOOP_VERSION ]]; then
            red "loop version $LOOP_VERSION does not match tag $TAG"
            exit 1
        fi
    else
        red "malformed loop version output"
        exit 1
    fi
}

# Needed for setting file timestamps to get reproducible archives.
BUILD_DATE="2020-01-01 00:00:00"
BUILD_DATE_STAMP="202001010000.00"

# reproducible_tar_gzip creates a reproducible tar.gz file of a directory. This
# includes setting all file timestamps and ownership settings uniformly.
function reproducible_tar_gzip() {
    local dir=$1
    local dst=$2
    local tar_cmd=tar
    local gzip_cmd=gzip

    # MacOS has a version of BSD tar which doesn't support setting the --mtime
    # flag. We need gnu-tar, or gtar for short to be installed for this script to
    # work properly.
    tar_version=$(tar --version 2>&1 || true)
    if [[ ! "$tar_version" =~ "GNU tar" ]]; then
        if ! command -v "gtar" >/dev/null 2>&1; then
            red "GNU tar is required but cannot be found!"
            red "On MacOS please run 'brew install gnu-tar' to install gtar."
            exit 1
        fi

        # We have gtar installed, use that instead.
        tar_cmd=gtar
    fi

    # On MacOS, the default BSD gzip produces a different output than the GNU
    # gzip on Linux. To ensure reproducible builds, we need to use GNU gzip.
    gzip_version=$(gzip --version 2>&1 || true)
    if [[ ! "$gzip_version" =~ "GNU" ]]; then
        if ! command -v "ggzip" >/dev/null 2>&1; then
            red "GNU gzip is required but cannot be found!"
            red "On MacOS please run 'brew install gzip' to install ggzip."
            exit 1
        fi

        # We have ggzip installed, use that instead.
        gzip_cmd=ggzip
    fi

    # Pin down the timestamp time zone.
    export TZ=UTC

    find "${dir}" -print0 | LC_ALL=C sort -r -z | $tar_cmd \
        "--mtime=${BUILD_DATE}" --no-recursion --null --mode=u+rw,go+r-w,a+X \
        --owner=0 --group=0 --numeric-owner -c -T - | $gzip_cmd -9n > "$dst"
}

# reproducible_zip creates a reproducible zip file of a directory. This
# includes setting all file timestamps.
function reproducible_zip() {
    local dir=$1
    local dst=$2

    # Pin down file name encoding and timestamp time zone.
    export TZ=UTC

    # Set the date of each file in the directory that's about to be packaged to
    # the same timestamp and make sure the same permissions are used everywhere.
    chmod -R 0755 "${dir}"
    touch -t "${BUILD_DATE_STAMP}" "${dir}"
    find "${dir}" -print0 | LC_ALL=C sort -r -z | xargs -0r touch \
        -t "${BUILD_DATE_STAMP}"

    find "${dir}" | LC_ALL=C sort -r | zip -o -X -r -@ "$dst"
}

##################
# Start Building #
##################

if [ -d "$BUILD_DIR" ]; then
    red "Build directory ${BUILD_DIR} already exists!"
    exit 1
fi

green " - Cloning to subdir ${BUILD_DIR} to get clean Git"
mkdir -p "$BUILD_DIR"
cd "$BUILD_DIR"
git clone --no-tags "$SCRIPT_DIR" .

# It is cloned without tags from the local dir and tags are pulled later
# from the upstream to make sure we have the same set of tags. Otherwise
# we can endup with different `git describe` and buildvcs info depending
# on local tags.
green " - Pulling tags from upstream"
git pull --tags https://github.com/lightninglabs/loop

# The cloned Git repo may be on wrong branch and commit.
commit=$(git --git-dir "${SCRIPT_DIR}/.git" rev-parse HEAD)
green " - Checkout commit ${commit} in ${BUILD_DIR}"
git checkout -b build-branch "$commit"

green " - Checking tag $1"
check_tag $1

PACKAGE=loop
FINAL_ARTIFACTS_DIR="${SCRIPT_DIR}/${PACKAGE}-${TAG}"
ARTIFACTS_DIR="${SCRIPT_DIR}/tmp-${PACKAGE}-${TAG}-$(date +%Y%m%d-%H%M%S)"
if [ -d "$ARTIFACTS_DIR" ]; then
    red "artifacts directory ${ARTIFACTS_DIR} already exists!"
    exit 1
fi
if [ -d "$FINAL_ARTIFACTS_DIR" ]; then
    red "final artifacts directory ${FINAL_ARTIFACTS_DIR} already exists!"
    exit 1
fi
green " - Creating artifacts directory ${ARTIFACTS_DIR}"
mkdir -p "$ARTIFACTS_DIR"
green " - Packaging vendor to ${ARTIFACTS_DIR}/vendor.tar.gz"
"$GO_CMD" mod vendor
reproducible_tar_gzip vendor "${ARTIFACTS_DIR}/vendor.tar.gz"
rm -r vendor

PACKAGESRC="${ARTIFACTS_DIR}/${PACKAGE}-source-${TAG}.tar.gz"
green " - Creating source archive ${PACKAGESRC}"
TMPSOURCETAR="${ARTIFACTS_DIR}/tmp-${PACKAGE}-source-${TAG}.tar"
PKGSRC="${PACKAGE}-source"
git archive -o "$TMPSOURCETAR" HEAD
cd "$ARTIFACTS_DIR"
mkdir "$PKGSRC"
tar -xf "$TMPSOURCETAR" -C "$PKGSRC"
cd "$PKGSRC"
reproducible_tar_gzip . "$PACKAGESRC"
cd ..
rm -r "$PKGSRC"
rm "$TMPSOURCETAR"

# If LOOPBUILDSYS is set the default list is ignored. Useful to release
# for a subset of systems/architectures.
SYS=${LOOPBUILDSYS:-"windows-amd64 linux-386 linux-amd64 linux-armv6 linux-armv7 linux-arm64 darwin-arm64 darwin-amd64 freebsd-amd64 freebsd-arm"}

PKG="github.com/lightninglabs/loop"
COMMIT=$(git describe --abbrev=40 --dirty)
GOLDFLAGS="-X $PKG/build.Commit=$COMMIT -buildid="

cd "$BUILD_DIR"
for i in $SYS; do
    OS=$(echo $i | cut -f1 -d-)
    ARCH=$(echo $i | cut -f2 -d-)
    ARM=

    if [[ $ARCH = "armv6" ]]; then
      ARCH=arm
      ARM=6
    elif [[ $ARCH = "armv7" ]]; then
      ARCH=arm
      ARM=7
    fi

    mkdir $PACKAGE-$i-$TAG
    cd $PACKAGE-$i-$TAG

    green "- Building: $OS $ARCH $ARM"
    for bin in loop loopd; do
        env CGO_ENABLED=0 GOOS=$OS GOARCH=$ARCH GOARM=$ARM "$GO_CMD" build -v -trimpath -ldflags "$GOLDFLAGS" "github.com/lightninglabs/loop/cmd/$bin"
    done
    cd ..

    if [[ $OS = "windows" ]]; then
        green "- Producing ZIP file ${ARTIFACTS_DIR}/${PACKAGE}-${i}-${TAG}.zip"
        reproducible_zip "${PACKAGE}-${i}-${TAG}" "${ARTIFACTS_DIR}/${PACKAGE}-${i}-${TAG}.zip"
    else
        green "- Producing TAR.GZ file ${ARTIFACTS_DIR}/${PACKAGE}-${i}-${TAG}.tar.gz"
        reproducible_tar_gzip "${PACKAGE}-${i}-${TAG}" "${ARTIFACTS_DIR}/${PACKAGE}-${i}-${TAG}.tar.gz"
    fi

    rm -r $PACKAGE-$i-$TAG
done

cd "$ARTIFACTS_DIR"
green "- Producing manifest-$TAG.txt"
shasum -a 256 * > manifest-$TAG.txt
shasum -a 256 manifest-$TAG.txt
cd ..

green "- Moving artifacts directory to final place ${FINAL_ARTIFACTS_DIR}"
mv "$ARTIFACTS_DIR" "$FINAL_ARTIFACTS_DIR"

green "- Removing the subdir used for building ${BUILD_DIR}"

rm -rf "$BUILD_DIR"
