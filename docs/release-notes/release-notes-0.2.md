# Loop Client Release Notes

- **Release date:** 2019-06-26
- **Release page:**
  [v0.2-alpha](https://github.com/lightninglabs/loop/releases/tag/v0.2-alpha)
- **Previous release:** [v0.1.3-beta](release-notes-0.1.3-beta.md)
- **Next release:** [v0.2.0-alpha](release-notes-0.2.0.md)

#### New Features

This is the first second release of Lightning Loop! This new release enables
**Loop In** on mainnet, and also exposes some additional code used to build the
`loop` daemon as an external package that others can use.
[Check out our latest blog post for more details on Loop In, and the future of Lightning Loop](https://blog.lightning.engineering/announcement/2019/06/25/loop-in.html).

**Loop In**

At a high-level a Loop In swap can be used to:
  * Refilling depleted channels with funds from cold-wallets or exchange
    withdrawals
  * Servicing off-chain Lightning withdrawals using on-chain payments, with no
    funds in channels required

#### Breaking Changes

#### Bug Fixes

  * As a failsafe payment method that can be used when channel liquidity along a
    route is insufficient

A new command has been added to the `loop` cli command to allow users to drive
Loop In swaps:
```
NAME:
   loop in - perform an on-chain to off-chain swap (loop in)

USAGE:
   loop in [command options] amt

DESCRIPTION:

    Send the amount in satoshis specified by the amt argument off-chain.

OPTIONS:
   --amt value  the amount in satoshis to loop in (default: 0)
   --external   expect htlc to be published externally
```

The `--external` argument allows the on-chain HTLC transacting to be published
externally. This allows for a number of use cases like using this address to
withdraw from an exchange into your Lightning channel!

[Additionally, the gRPC API (and REST!) docs have also been updated](https://lightning.engineering/loop).

Loop there it is!! ⚡️🔁

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
`manifest-v0.2-alpha.txt` and `manifest-v0.2-alpha.txt.sig` are in the current
directory) with:
```
gpg --verify manifest-v0.2-alpha.txt
```

You should see the following if the verification was successful:
```
gpg: assuming signed data in 'manifest-v0.2-alpha.txt'
gpg: Signature made Wed Jun 26 09:56:58 2019 PDT
gpg:                using RSA key F8037E70C12C7A263C032508CE58F7F8E20FD9A2
gpg: Good signature from "Olaoluwa Osuntokun <laolu32@gmail.com>" [ultimate]
```

That will verify the signature on the main manifest page which ensures integrity
and authenticity of the binaries you've downloaded locally. Next, depending on
your operating system you should then re-calculate the `sha256` sum of the
binary, and compare that with the following hashes (which are included in the
manifest file):
```
de97a2e33fcce9650911f8976ed9089d061a1d5fb374bdfd3be0d6101871585d  loop-darwin-386-v0.2-alpha.tar.gz
666a5910757cb15dd2a62ac009b31359a97f4f1c83803d4028d10d23db6ee587  loop-darwin-amd64-v0.2-alpha.tar.gz
4a06d43e48e7537975a92d25ac187b78a74426976ffd36bb1efaeb83bae7aa4f  loop-dragonfly-amd64-v0.2-alpha.tar.gz
f6cf356db4060d8c90b91dc1d8547472b255ceb8aae2197f531a177a285f60fc  loop-freebsd-386-v0.2-alpha.tar.gz
74fdb8d3a8e0a91cfeabe5e06f2be386a68e4524fa42d9c042b9a2030499a76d  loop-freebsd-amd64-v0.2-alpha.tar.gz
d500f36eefc1e1ee63b8259d4eefcd64f34b295781956559bd52389b5a37a98b  loop-freebsd-arm-v0.2-alpha.tar.gz
d2d01f9a8d1483173c5f1618477ff1929e8aa5e322849091d7b86694fedbf0bb  loop-linux-386-v0.2-alpha.tar.gz
da92445ec5e754da3f379e85310d697f3609a17e6639d1251a479a09fc0c16e1  loop-linux-amd64-v0.2-alpha.tar.gz
04edae2cbdb6943b70b13cbc678901d26f4f06767f60f591f7c7d791c8b5925f  loop-linux-arm64-v0.2-alpha.tar.gz
5fae0185f8849364af9b998609d002f5e226d3e937dabfd56d1474415f6555db  loop-linux-armv6-v0.2-alpha.tar.gz
931a43c8a936db0a3a6a3d099b67d953b69344259bfa7d37faac985341ff27e9  loop-linux-armv7-v0.2-alpha.tar.gz
757ae30b349f8fc209f7df20e1b6010814d0dd279a3717b8c7ba5cb7a092103b  loop-linux-mips64-v0.2-alpha.tar.gz
900c8ce735ac94f1d35c179e4387cc9292f261be685eb57b865d772bd668d8ec  loop-linux-mips64le-v0.2-alpha.tar.gz
aeb3d2f882405308827974515ea5d652d5e6d72188165ab315dbbb4c7402bc3f  loop-linux-ppc64-v0.2-alpha.tar.gz
0d072fd9ed648de679f46ee959a3aef1ae9e025d756140e432df2a51bfe57221  loop-netbsd-386-v0.2-alpha.tar.gz
f26cedf2e8a38b175b47a6b6101edab99033e4776c6de21306cdc793e6dc0b0b  loop-netbsd-amd64-v0.2-alpha.tar.gz
3ea2bb1482c78d2bb45358c3de7961670b5ed64a231ec9a2dc2b6ba13a3e76a6  loop-openbsd-386-v0.2-alpha.tar.gz
7a67ae9805beb8e43bd257e8355f8c471bbc740b1b2f1bbf8f834d50312299fe  loop-openbsd-amd64-v0.2-alpha.tar.gz
8f15d5c5f56bd19952361ee27b1821306d634bbe8799e52ab36a09f8c151ff6d  loop-source-v0.2-alpha.tar.gz
99de0a0e0464a5d7910e6ec4c5cdc881db278bf1bbb6095d5aec7c7a033aca0f  loop-windows-386-v0.2-alpha.zip
fb1e4dca3728a2608dce560385bf6e07cb319d27f4d7cfeb0a4aaf00b1cc2a4c  loop-windows-amd64-v0.2-alpha.zip
47b208033931c22fd6b1b23b723d49e18359709cb6b29a3f2623e6f80179bfbf  vendor.tar.gz
```

One can use the `shasum -a 256 <file name here>` tool in order to re-compute the
`sha256` hash of the target binary for your operating system. The produced hash
should be compared with the hashes listed above and they should match *exactly*.

Finally, you can also verify the _tag_ itself with the following command:
```
git verify-tag v0.2-alpha
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
`loop-source-v0.2-alpha.tar.gz` are in the current directory:
```
tar -xvzf vendor.tar.gz
tar -xvzf loop-source-v0.2-alpha.tar.gz
GO111MODULE=on go install -v -mod=vendor -ldflags "-X github.com/lightninglabs/loop.Commit=v0.2-alpha" ./cmd/loop
GO111MODULE=on go install -v -mod=vendor -ldflags "-X github.com/lightninglabs/loop.Commit=v0.2-alpha" ./cmd/loopd
```

The `-mod=vendor` flag tells the `go build` command that it doesn't need to
fetch the dependencies, and instead, they're all enclosed in the local vendor
directory.

#### Contributors (Alphabetical Order)

- Olaoluwa Osuntokun
