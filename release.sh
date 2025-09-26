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
mkdir -p $BUILD_DIR
cd $BUILD_DIR
git clone --tags "$SCRIPT_DIR" .

TAG=''

check_tag() {
    # If no tag specified, use date + version otherwise use tag.
    if [[ $1x = x ]]; then
        TAG=`date +%Y%m%d-%H%M%S`

        return
    fi

    TAG=$1

    # If a tag is specified, ensure that tag is present and checked out.
    if [[ $TAG != $(git describe) ]]; then
        echo "tag $TAG not checked out"
        exit 1
    fi

    # Verify that it is signed if it is a real tag. If the tag looks like the
    # output of "git describe" for an untagged commit, skip verification.
    # The pattern is: <tag_name>-<number_of_commits>-g<abbreviated_commit_hash>
    # Example: "v0.31.2-beta-122-g8c6b73c".
    if [[ $TAG =~ -[0-9]+-g([0-9a-f]+)$ ]]; then
        # This looks like a "git describe" output. Make sure the hash
        # described is a prefix of the current commit.
        DESCRIBED_HASH=${BASH_REMATCH[1]}
        CURRENT_HASH=$(git rev-parse HEAD)
        if [[ $CURRENT_HASH != $DESCRIBED_HASH* ]]; then
            echo "Described hash $DESCRIBED_HASH is not a prefix of current commit $CURRENT_HASH"
            exit 1
        fi

        return
    fi

    if ! git verify-tag $TAG; then
        echo "tag $TAG not signed"
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
            echo "loop version $LOOP_VERSION does not match tag $TAG"
            exit 1
        fi
    else
        echo "malformed loop version output"
        exit 1
    fi
}

check_tag $1

go mod vendor
tar -cvzf vendor.tar.gz vendor

PACKAGE=loop
ARTIFACTS_DIR="${SCRIPT_DIR}/${PACKAGE}-${TAG}"
mkdir -p $ARTIFACTS_DIR

cp vendor.tar.gz $ARTIFACTS_DIR/
rm vendor.tar.gz
rm -r vendor

PACKAGESRC="${ARTIFACTS_DIR}/${PACKAGE}-source-${TAG}.tar"
git archive -o $PACKAGESRC HEAD
gzip -f $PACKAGESRC > "$PACKAGESRC.gz"

# If LOOPBUILDSYS is set the default list is ignored. Useful to release
# for a subset of systems/architectures.
SYS=${LOOPBUILDSYS:-"windows-amd64 linux-386 linux-amd64 linux-armv6 linux-armv7 linux-arm64 darwin-arm64 darwin-amd64 freebsd-amd64 freebsd-arm"}

PKG="github.com/lightninglabs/loop"
COMMIT=$(git describe --abbrev=40 --dirty)
COMMITFLAGS="-X $PKG/build.Commit=$COMMIT"

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

    echo "Building:" $OS $ARCH $ARM
    for bin in loop loopd; do
        env CGO_ENABLED=0 GOOS=$OS GOARCH=$ARCH GOARM=$ARM go build -v -ldflags "$COMMITFLAGS" "github.com/lightninglabs/loop/cmd/$bin"
    done
    cd ..

    if [[ $OS = "windows" ]]; then
        zip -r "${ARTIFACTS_DIR}/${PACKAGE}-${i}-${TAG}.zip" "${PACKAGE}-${i}-${TAG}"
    else
        tar -cvzf "${ARTIFACTS_DIR}/${PACKAGE}-${i}-${TAG}.tar.gz" "${PACKAGE}-${i}-${TAG}"
    fi

    rm -r $PACKAGE-$i-$TAG
done

cd "$ARTIFACTS_DIR"

shasum -a 256 * > manifest-$TAG.txt
