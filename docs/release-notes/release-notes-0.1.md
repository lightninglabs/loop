# Loop Client Release Notes

- **Release date:** 2019-03-21
- **Release page:**
  [v0.1-alpha](https://github.com/lightninglabs/loop/releases/tag/v0.1-alpha)
- **Previous release:** None
- **Next release:** [v0.1.1-alpha](release-notes-0.1.1.md)

#### New Features

This is the first major release of Lightning Loop! This release is the first of
many planned, and includes the initial base functionality for the system.
[Check out our blog post for a high level overview on how the Loop service works](https://blog.lightning.engineering/posts/2019/03/20/loop.html).
With this new release, users can use Loop Out to free up inbound receiving
bandwidth in their channels and also send coins on chain from their existing
channel.

The Lightning Loop client software is similar to `lnd`. There's the primary
daemon `loopd`, and its command-line interface `loop`.
[Check out the instructions in the `README` to get started](https://github.com/lightninglabs/loop/blob/master/README.md).
The `loop` command is very simple and at initial release has the following
commands:
```
⛰   loop -h
NAME:
   loop - control plane for your loopd

USAGE:
   loop [global options] command [command options] [arguments...]

VERSION:
   0.1.0-alpha commit=v0.1-alpha

COMMANDS:
     out      perform an off-chain to on-chain swap (looping out)
     terms    Display the current swap terms imposed by the server.
     monitor  monitor progress of any active swaps
     quote    get a quote for the cost of a swap
     help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --loopd value  loopd daemon address host:port (default: "localhost:11010")
   --help, -h     show help
   --version, -v  print the version
```

You'll most frequently be using `loop out`. A sample run looks something like:
```
loop out 500000 --addr=bc1qnrhawnjr7fn3gd09s8gltzzjdfsxsrm2k64ze7
```

By specifying the optional `addr` flag, I tell `loopd` that I'd like to have the
funds sent to that target address. This lets one do cool things like send
directly from your channel into cold storage, or even deposit directly into an
exchange or another wallet under one's control.

Once the loop has been initiated, you can use `loop monitor` to check on its
status.

The other primary way to
[interact with Lightning Loop is via the gRPC API](https://lightning.engineering/loop).
This lets existing lapps, services, and businesses drive Loop programmatically
in a similar fashion to how they interact with `lnd` today. We'll have more
example uses cases, documentation, and explainers in the near future.

Loop there it is!! ⚡️🔁

#### Breaking Changes

#### Bug Fixes

#### Maintenance

**Verifying the Release**

In order to verify the release, you'll need to have `gpg` or `gpg2` installed on
your system. Once you've obtained a copy (and hopefully verified that as well),
you'll first need to import the keys that have signed this release if you
haven't done so already:
```
curl https://keybase.io/roasbeef/pgp_keys.asc | gpg --import
```

Once you have his PGP key you can verify the release (assuming
`manifest-v0.1-alpha.txt` and `manifest-v0.1-alpha.txt.sig` are in the current
directory) with:
```
gpg --verify manifest-v0.1-alpha.txt
```

You should see the following if the verification was successful:
```
gpg: assuming signed data in 'manifest-v0.1-alpha.txt'
gpg: Signature made Wed Mar 20 20:22:49 2019 PDT
gpg:                using RSA key F8037E70C12C7A263C032508CE58F7F8E20FD9A2
gpg: Good signature from "Olaoluwa Osuntokun <laolu32@gmail.com>" [ultimate]
```

That will verify the signature on the main manifest page which ensures integrity
and authenticity of the binaries you've downloaded locally. Next, depending on
your operating system you should then re-calculate the `sha256` sum of the
binary, and compare that with the following hashes (which are included in the
manifest file):
```
5c63d870d007d4dd9d0480a0ebc1fee83f1099acac12a4bdd121d3882b772bfb  loop-darwin-386-v0.1-alpha.tar.gz
98fc299e107c89df4875af0f2fe46957b228321036c60661507ecdbb9ba1a56a  loop-darwin-amd64-v0.1-alpha.tar.gz
761c5af296e62b2eaa9421e9d36a258e9f435a2bc53df33b950b41d3a9eef89d  loop-dragonfly-amd64-v0.1-alpha.tar.gz
269997eba3daebeeac7edb82739b3406df3e52b3345c61124c60b145b9080c0f  loop-freebsd-386-v0.1-alpha.tar.gz
38fb7a32c9c36f80cac9a47e448338dedde53554f569454202236d278654eadf  loop-freebsd-amd64-v0.1-alpha.tar.gz
7226a8c5171c6e10adf754d1c3acd81dee84497d170877981ea441108368fee7  loop-freebsd-arm-v0.1-alpha.tar.gz
e07552fd11967743262a678060369f5fbe17bcde3c4ee6cc21196618248afb9c  loop-linux-386-v0.1-alpha.tar.gz
816a7050c91652a8e5b881e939c1bc70e06fe23e8309ac80ef5d38fda06969b7  loop-linux-amd64-v0.1-alpha.tar.gz
75a9ce3f605359735d39d9a0fb5037676c6ac29a70d7db5bc75fc1e5e3fcc465  loop-linux-arm64-v0.1-alpha.tar.gz
b7c7668c90f1d67689a9e557346e32bf10c169ace220cd507ae4ee63f64b12d1  loop-linux-armv6-v0.1-alpha.tar.gz
8aad519ff927643c35968bc9eb4e91a5a1f063ebf60e50b12c05f745d3602540  loop-linux-armv7-v0.1-alpha.tar.gz
5b3be8f2ab4acff7e854a794ecde59abd7feed3002bb13177f89c1cd1c9419e7  loop-linux-mips64-v0.1-alpha.tar.gz
aa3da5049eb2d411912aba8386dcc241f9db2b41dd4571ddbea80cf07f3fbc20  loop-linux-mips64le-v0.1-alpha.tar.gz
1a6a8e8e93df6c02eac1e61d4e9f3a518bc2c2a096deb0b77f0e5a6c0afb553d  loop-linux-ppc64-v0.1-alpha.tar.gz
85e4f4e32783aad5e7e53125ca4c2efecaf997b24e81e54db0cc380cdf876cf9  loop-netbsd-386-v0.1-alpha.tar.gz
8896df3499e9287177b5eacb2fc6b964e6dec698807f49582cbccbd3b7088878  loop-netbsd-amd64-v0.1-alpha.tar.gz
133b069ad151adeac9c62596ba507f9ed425cfe6670a03e2e5dea6316e522f18  loop-openbsd-386-v0.1-alpha.tar.gz
a9d1f80d34f698450d413844ff1a4536a4377faa6debd7c16bcfde94964cd628  loop-openbsd-amd64-v0.1-alpha.tar.gz
94c35abdcafeebb17f19850b5e88031926116cce8bd22cdf56d905716a0063d8  loop-source-v0.1-alpha.tar.gz
5cd5171f469e639ece6b37a2ff35bae3dac6d1b0a9490ed9e3f90e4f1fcadb97  loop-windows-386-v0.1-alpha.zip
b139416e426868b060ea6fd28ba840398a6e45ad492eac50297462d4391bc0ce  loop-windows-amd64-v0.1-alpha.zip
e2ab0d8f7ec07468ea46e551f17521799147a01f48a3f2c5b1c2d62efd9512a5  vendor.tar.gz
```

One can use the `shasum -a 256 <file name here>` tool in order to re-compute the
`sha256` hash of the target binary for your operating system. The produced hash
should be compared with the hashes listed above and they should match *exactly*.

Finally, you can also verify the _tag_ itself with the following command:
```
git verify-tag v0.1-alpha
```

**Building the Contained Release**

With this new version of `loop`, we've modified our release process to ensure
the bundled release is now _fully self contained_. As a result, with only the
attached payload with this release, users will be able to rebuild the target
release themselves without having to fetch any of the dependancies. Note that at
this stage, binaries aren't yet fully reproducible (even with `go modules` ).
This is due to the fact that by default,
[Go will include the full directory path where the binary was built in the binary itself](https://github.com/golang/go/issues/16860).
As a result, unless your file system exactly mirrors the machine used to build
the binary, you'll get a different binary, as it includes artifacts from your
local file system. This will be fixed in `go1.13`, and before then we may modify
our release system to do this automatically.

In order to re-build from scratch, assuming that `vendor.tar.gz` and
`loop-source-v0.1-alpha.tar.gz` are in the current directory:
```
tar -xvzf vendor.tar.gz
tar -xvzf loop-source-v0.1-alpha.tar.gz
GO111MODULE=on go install -v -mod=vendor -ldflags "-X github.com/lightninglabs/loop.Commit=v0.1-alpha" ./cmd/loop
GO111MODULE=on go install -v -mod=vendor -ldflags "-X github.com/lightninglabs/loop.Commit=v0.1-alpha" ./cmd/loopd
```

The `-mod=vendor` flag tells the `go build` command that it doesn't need to
fetch the dependencies, and instead, they're all enclosed in the local vendor
directory.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Joost Jager
- Olaoluwa Osuntokun
- Wilmer Paulino
