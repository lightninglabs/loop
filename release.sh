#!/bin/bash

# Simple bash script to build basic loop tools for all the platforms
# we support with the golang cross-compiler.
#
# Copyright (c) 2016 Company 0, LLC.
# Use of this source code is governed by the ISC
# license.

# Exit on errors.
set -e

# If no tag specified, use date + version otherwise use tag.
if [[ $1x = x ]]; then
    DATE=`date +%Y%m%d`
    VERSION="01"
    TAG=$DATE-$VERSION
else
    TAG=$1

    # If a tag is specified, ensure that that tag is present and checked out.
    if [[ $TAG != $(git describe) ]]; then
        echo "tag $TAG not checked out"
        exit 1
    fi

    # Verify that it is signed.
    if ! git verify-tag $TAG; then 
        echo "tag $TAG not signed"
        exit 1
    fi

    # Build loop to extract version.
    make

    # Extract version command output.
    LOOP_VERSION_OUTPUT=`./loopd-debug --version`

    # Use a regex to isolate the version string.
    LOOP_VERSION_REGEX="version (.+) "
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
fi

go mod vendor
tar -cvzf vendor.tar.gz vendor

PACKAGE=loop
MAINDIR=$PACKAGE-$TAG
mkdir -p $MAINDIR

cp vendor.tar.gz $MAINDIR/
rm vendor.tar.gz
rm -r vendor

PACKAGESRC="$MAINDIR/$PACKAGE-source-$TAG.tar"
git archive -o $PACKAGESRC HEAD
gzip -f $PACKAGESRC > "$PACKAGESRC.gz"

cd $MAINDIR

# If LOOPBUILDSYS is set the default list is ignored. Useful to release
# for a subset of systems/architectures.
SYS=${LOOPBUILDSYS:-"windows-amd64 linux-386 linux-amd64 linux-armv6 linux-armv7 linux-arm64 darwin-arm64 darwin-amd64 freebsd-amd64 freebsd-arm"}

# Use the first element of $GOPATH in the case where GOPATH is a list
# (something that is totally allowed).
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
    env GOOS=$OS GOARCH=$ARCH GOARM=$ARM go build -v -ldflags "$COMMITFLAGS" github.com/lightninglabs/loop/cmd/loop
    env GOOS=$OS GOARCH=$ARCH GOARM=$ARM go build -v -ldflags "$COMMITFLAGS" github.com/lightninglabs/loop/cmd/loopd
    cd ..

    if [[ $OS = "windows" ]]; then
	zip -r $PACKAGE-$i-$TAG.zip $PACKAGE-$i-$TAG
    else
	tar -cvzf $PACKAGE-$i-$TAG.tar.gz $PACKAGE-$i-$TAG
    fi

    rm -r $PACKAGE-$i-$TAG
done

shasum -a 256 * > manifest-$TAG.txt
