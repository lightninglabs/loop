#!/usr/bin/env python3

"""
check_multiarch_image verifies that a multi-architecture container image
really contains binaries for every architecture its manifest advertises.

It reads an OCI image layout tarball as produced by

    docker buildx build --platform linux/amd64,linux/arm64 \\
        --output type=oci,dest=image.tar .

and, for every platform entry in the image index, checks that:

  * the entry's image config agrees with the platform in the index,
  * the ELF header of every checked binary describes the entry's
    architecture, and
  * no two entries are built from an identical list of layers.

The middle check is the one that matters. A Dockerfile that pins its final
stage to ${BUILDPLATFORM} produces an index that advertises linux/arm64
while every layer holds amd64 binaries, and buildx reports success. See
https://github.com/lightninglabs/loop/issues/1211. Checking a binary from
the base image, such as /bin/busybox, as well as the ones the build
produced covers both halves of that mistake.

Everything is done by inspecting the artifact, never by running it. Running
the image cannot detect this bug on an amd64 host: the amd64 binaries inside
the mislabelled arm64 entry execute natively, so the check would pass.
"""

import argparse
import gzip
import io
import json
import sys
import tarfile

# The ELF identity each architecture must have, as
# (EI_CLASS, EI_DATA, e_machine). EI_CLASS is 1 for 32 bit and 2 for 64 bit,
# EI_DATA is 1 for little endian and 2 for big endian. An architecture that
# is not listed is reported as an error rather than skipped, so that adding a
# platform to the build cannot silently bypass this check.
ELF_IDENTITIES = {
    "386": (1, 1, 0x03),
    "amd64": (2, 1, 0x3E),
    "arm": (1, 1, 0x28),
    "arm64": (2, 1, 0xB7),
    "ppc64le": (2, 1, 0x15),
    "riscv64": (2, 1, 0xF3),
    "s390x": (2, 2, 0x16),
}

# Media types of layers this script knows how to unpack.
GZIP_LAYERS = (
    "application/vnd.oci.image.layer.v1.tar+gzip",
    "application/vnd.docker.image.rootfs.diff.tar.gzip",
)
PLAIN_LAYERS = (
    "application/vnd.oci.image.layer.v1.tar",
)

INDEX_TYPES = (
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
)
MANIFEST_TYPES = (
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
)

# Length of the ELF header prefix that is kept for each checked file. Only
# the first 20 bytes are read from it, but a short read of a tiny file is
# easier to recognise with a little more context.
HEADER_BYTES = 64


class CheckError(Exception):
    """CheckError is a fatal problem that prevents the image being checked."""


class Layout:
    """Layout gives access to the blobs of an OCI layout inside a tarball."""

    def __init__(self, tar):
        """Wrap an already opened tarfile holding an OCI image layout."""
        self._tar = tar

    def _member(self, name):
        """Return an open file for a layout member, or None if absent."""
        try:
            return self._tar.extractfile(name)
        except KeyError:
            return None

    def blob(self, digest):
        """Return the raw bytes of the blob with the given digest."""
        algo, hexdigest = digest.split(":", 1)
        handle = self._member(f"blobs/{algo}/{hexdigest}")
        if handle is None:
            raise CheckError(f"blob {digest} missing from layout")

        return handle.read()

    def json_blob(self, digest):
        """Return the blob with the given digest, parsed as JSON."""
        return json.loads(self.blob(digest))

    def root_index(self):
        """Return the image index describing all platforms in the layout."""
        handle = self._member("index.json")
        if handle is None:
            raise CheckError("index.json missing: not an OCI image layout")

        index = json.loads(handle.read())

        # buildx wraps the real index in index.json when the export is named,
        # and writes it directly otherwise. Descend until the manifest list
        # holds image manifests.
        seen = set()
        while True:
            children = index.get("manifests", [])
            nested = [c for c in children if c["mediaType"] in INDEX_TYPES]
            if len(children) != 1 or not nested:
                return index

            digest = nested[0]["digest"]
            if digest in seen:
                raise CheckError("image index refers to itself")

            seen.add(digest)
            index = self.json_blob(digest)


def platform_name(platform):
    """Render an OCI platform object as os/arch or os/arch/variant."""
    name = f"{platform.get('os')}/{platform.get('architecture')}"
    if platform.get("variant"):
        name += "/" + platform["variant"]

    return name


def is_attestation(descriptor):
    """Report whether a descriptor is an attestation rather than an image."""
    platform = descriptor.get("platform", {})

    return platform.get("architecture") == "unknown"


def layer_members(layout, layer):
    """Yield (member, handle) for every entry of a single image layer.

    The handle is None for anything that is not a regular file, which the
    caller needs in order to notice a path being replaced by a symlink or a
    directory.
    """
    media_type = layer["mediaType"]
    raw = layout.blob(layer["digest"])
    if media_type in GZIP_LAYERS:
        raw = gzip.decompress(raw)
    elif media_type not in PLAIN_LAYERS:
        raise CheckError(f"unsupported layer media type {media_type}")

    with tarfile.open(fileobj=io.BytesIO(raw)) as layer_tar:
        for member in layer_tar:
            handle = layer_tar.extractfile(member) if member.isfile() else None

            yield member, handle


def image_path(name):
    """
    Normalise a tar member name to an absolute image path.

    Note that the leading "./" has to be removed as a prefix rather than
    with lstrip, which takes a set of characters and would eat the leading
    dot of a whiteout sitting at the root of the layer.
    """
    while name.startswith("./"):
        name = name[2:]

    name = name.strip("/")
    if name in ("", "."):
        return "/"

    return "/" + name


def hide_below(found, directory):
    """Drop everything the lower layers put inside a directory."""
    prefix = "/" if directory in ("", "/") else directory + "/"
    for below in [p for p in found if p.startswith(prefix)]:
        del found[below]


def hide(found, path):
    """Drop a path, and anything below it, from the discovered files."""
    found.pop(path, None)
    hide_below(found, path)


def find_binaries(layout, layers, wanted):
    """
    Return a mapping of wanted image paths to their leading header bytes.

    Layers are applied in order and overlay deletions are honoured, so a
    path is only reported when the assembled image really holds a regular
    file there. A whiteout, an opaque directory whiteout, or a replacement
    by a file, symlink or hard link all drop what the lower layers put at
    that path, including anything they put underneath it when the path was
    a directory. Only a directory merges with the layers below it.

    Symlinks and hard links are not followed, so a checked path that turns
    into one is reported as missing rather than resolved to its target.

    Paths are given as absolute image paths, e.g. /bin/loopd.
    """
    targets = {image_path(path) for path in wanted}
    found = {}
    for layer in layers:
        for member, handle in layer_members(layout, layer):
            path = image_path(member.name)
            directory, _, base = path.rpartition("/")

            # An opaque whiteout hides everything the lower layers put in
            # this directory, while the directory itself stays.
            if base == ".wh..wh..opq":
                hide_below(found, directory)
                continue

            # A plain whiteout hides one name, and its contents if that
            # name was a directory.
            if base.startswith(".wh."):
                hide(found, image_path(directory + "/" + base[len(".wh."):]))
                continue

            # A directory merges with the lower layers. Anything else takes
            # the path over, so whatever was there before is gone.
            if member.isdir():
                continue

            hide(found, path)

            if handle is not None and path in targets:
                found[path] = handle.read(HEADER_BYTES)

    return found


def elf_identity(header):
    """
    Return (EI_CLASS, EI_DATA, e_machine) for an ELF header, else None.

    None means the bytes are not an ELF header at all, or carry a class or
    endianness that ELF does not define.
    """
    if len(header) < 20 or header[:4] != b"\x7fELF":
        return None

    ei_class, ei_data = header[4], header[5]
    if ei_class not in (1, 2) or ei_data not in (1, 2):
        return None

    order = "little" if ei_data == 1 else "big"

    return ei_class, ei_data, int.from_bytes(header[18:20], order)


def describe_identity(identity):
    """Name the architecture with this ELF identity, or describe it raw."""
    for arch, expected in ELF_IDENTITIES.items():
        if expected == identity:
            return arch

    ei_class, ei_data, machine = identity
    bits = {1: "32 bit", 2: "64 bit"}[ei_class]
    order = {1: "little endian", 2: "big endian"}[ei_data]

    return f"an unrecognised {bits} {order} ELF (e_machine 0x{machine:02x})"


def check_entry(layout, descriptor, binaries, verbose=True):
    """
    Check one platform entry of the index and return a list of problems.

    An empty list means the entry advertises an architecture that its config
    and its binaries both agree with.
    """
    platform = descriptor["platform"]
    name = platform_name(platform)
    arch = platform["architecture"]
    problems = []

    manifest = layout.json_blob(descriptor["digest"])
    config = layout.json_blob(manifest["config"]["digest"])

    if config.get("architecture") != arch or config.get("os") != platform.get("os"):
        problems.append(
            f"{name}: index says {name} but image config says "
            f"{config.get('os')}/{config.get('architecture')}"
        )

    want = ELF_IDENTITIES.get(arch)
    if want is None:
        problems.append(
            f"{name}: no ELF identity is known for this architecture, add "
            f"it to ELF_IDENTITIES so the binaries can be checked"
        )

    headers = find_binaries(layout, manifest["layers"], binaries)
    for path in binaries:
        header = headers.get(path)
        if header is None:
            problems.append(f"{name}: {path} not found in any layer")
            continue

        got = elf_identity(header)
        if got is None:
            problems.append(f"{name}: {path} is not an ELF binary")
            continue

        if want is not None and got != want:
            problems.append(
                f"{name}: {path} is {describe_identity(got)}, expected "
                f"{arch}"
            )
            continue

        if verbose:
            print(f"  ok  {name:<14} {path:<14} {arch} (e_machine "
                  f"0x{got[2]:02x})")

    return problems


def check_layers_identical(layers):
    """
    Return a problem for each pair of entries built from the same layers.

    Sharing an individual layer between platforms is legitimate and is not
    reported: a layer holding only architecture neutral files has the same
    digest whichever platform it was built for. Two entries built from an
    identical list of layers are a different matter, because they are then
    the same filesystem published under two architecture labels, of which at
    most one can be true for an image holding compiled binaries.
    """
    problems = []
    names = sorted(layers)
    for index, first in enumerate(names):
        for second in names[index + 1:]:
            if layers[first] == layers[second]:
                problems.append(
                    f"{first} and {second} are built from an identical list "
                    f"of {len(layers[first])} layers, so they are one "
                    f"filesystem published under two architecture labels"
                )

    return problems


def main():
    """Parse arguments, check the image layout and set the exit status."""
    parser = argparse.ArgumentParser(
        description="Verify a multi-arch OCI image really is multi-arch.",
    )
    parser.add_argument(
        "tarball",
        help="OCI image layout tarball (buildx --output type=oci,dest=...)",
    )
    parser.add_argument(
        "--platforms",
        default="linux/amd64,linux/arm64",
        help="comma separated platforms that must be present. Every entry "
             "the index holds is checked whether it is named here or not, "
             "so an unexpected platform fails rather than slips through",
    )
    parser.add_argument(
        "--binaries",
        default="/bin/loopd,/bin/loop,/bin/busybox",
        help="comma separated image paths whose ELF headers must match",
    )
    parser.add_argument(
        "--digests-out",
        help="write '<platform> <manifest digest>' lines to this file, so "
             "that the artifact that was checked can be matched against the "
             "one a registry ended up with",
    )
    args = parser.parse_args()

    expected = [p.strip() for p in args.platforms.split(",") if p.strip()]
    binaries = [b.strip() for b in args.binaries.split(",") if b.strip()]

    print(f"checking {args.tarball}")
    print(f"expecting {', '.join(expected)}")

    try:
        tar = tarfile.open(args.tarball)
    except (OSError, tarfile.TarError) as err:
        raise CheckError(f"cannot read {args.tarball} as a tarball: {err}")

    problems = []
    with tar:
        layout = Layout(tar)
        index = layout.root_index()

        entries = {}
        layers = {}
        for descriptor in index.get("manifests", []):
            if descriptor["mediaType"] not in MANIFEST_TYPES:
                continue

            if is_attestation(descriptor):
                continue

            name = platform_name(descriptor["platform"])
            entries[name] = descriptor
            manifest = layout.json_blob(descriptor["digest"])
            layers[name] = [layer["digest"] for layer in manifest["layers"]]

        for name in expected:
            if name not in entries:
                problems.append(
                    f"{name}: no entry in the image index, found "
                    f"{', '.join(sorted(entries)) or 'none'}"
                )

        problems += check_layers_identical(layers)

        # Every entry is checked, not just the expected ones. An entry that
        # nobody asked for still gets the release tag, so leaving it
        # unexamined would let it through both this check and the digest
        # list written below.
        for name in sorted(entries):
            problems += check_entry(layout, entries[name], binaries)

        checked = len(entries)

        digests = sorted(
            f"{name} {entries[name]['digest']}" for name in entries
        )

    # The digest list is only written once everything has passed, so that a
    # failed run cannot leave one behind for a later step to trust.
    if not problems and args.digests_out:
        with open(args.digests_out, "w") as handle:
            handle.write("".join(line + "\n" for line in digests))

        print(f"\nwrote {len(digests)} manifest digests to "
              f"{args.digests_out}")

    if problems:
        sys.stdout.flush()
        print(f"\nFAILED: {len(problems)} problem(s) found:", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)

        return 1

    print(f"\nOK: {checked} platforms, all binaries match their label")

    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except CheckError as err:
        print(f"error: {err}", file=sys.stderr)
        sys.exit(2)
