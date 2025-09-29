# Reproducible Builds

## Building with Docker

To create a Loop release with binaries that are identical to an official
release, run the following command (available since release `v0.31.3-beta`):

```bash
make docker-release tag=<tag-of-release>
```

This command will create a directory named `loop-<tag-of-release>` containing
the source archive, vendored dependencies, and the built binaries packaged in
`.tar.gz` or `.zip` format. It also creates a manifest file with `SHA-256`
checksums for all release files.

For example:

```bash
make docker-release tag=v0.31.3-beta
```

This will create the release artifacts in the `loop-v0.31.3-beta` directory.

If you want to build from an untagged commit, first check it out, then use the
output of `git describe --abbrev=10` as the tag:

```bash
git describe --abbrev=10
# v0.31.2-beta-135-g35d0fa26ac

make docker-release tag=v0.31.2-beta-135-g35d0fa26ac
```

You can filter the target platforms to speed up the build process. For example,
to build only for `linux-amd64`:

```bash
make docker-release buildsys=linux-amd64 tag=v0.31.3-beta
```

Or for multiple platforms:

```bash
make docker-release buildsys='linux-amd64 windows-amd64' tag=v0.31.3-beta
```

Note: inside Docker the current directory is mapped as `/repo` and it might
mention `/repo` as parts of file paths.

## Building on the Host

You can also build a release on your host system without Docker. You will need
to install the Go version specified in the `go.mod` file, as well as a few
other tools:

```bash
sudo apt-get install build-essential git make zip perl gpg
```

Add GPG key of Alex Bosworth to verify release tag signature:
```bash
gpg --keyserver keys.openpgp.org --recv-keys DE23E73BFA8A0AD5587D2FCDE80D2F3F311FD87E
```

Then, run the `release.sh` script directly:

```bash
./release.sh <tag-of-release>
```

To filter the target platforms, pass them as a space-separated list in the
`LOOPBUILDSYS` environment variable:

```bash
LOOPBUILDSYS='linux-amd64 windows-amd64' ./release.sh v0.31.3-beta
```

This will produce the same artifacts in a `loop-<tag-of-release>` directory as
the `make docker-release` command. The latter simply runs the `release.sh`
script inside a Docker container.
