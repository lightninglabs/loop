# Loop Client Release Notes

- **Release date:** 2019-04-17
- **Release page:**
  [v0.1.1-alpha](https://github.com/lightninglabs/loop/releases/tag/v0.1.1-alpha)
- **Previous release:** [v0.1-alpha](release-notes-0.1.md)
- **Next release:** [v0.1.2-alpha](release-notes-0.1.2.md)

#### New Features

This is a minor release of the Lightning Loop Go client.

Includes changes to:
- Add testnet support for Loop In (on-chain to off-chain swaps)
- Add amt as a keyword argument

The Loop In implementation can be kicked off using the `loop in ` command:
```
⛰   loop in -h
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

The `--external` flag is of note as it allows the HTLC to be paid by a wallet
other then `lnd`. This allows for users to do things like top-off channel from
_another wallet_, or _withdrawal_ funds from an exchange directly into one's
channel. It can also be used to obtain an address to give to someone else, which
once paid and Looped In, is credited to your channel!

#### Breaking Changes

#### Bug Fixes

- Fix monitoring exception

#### Maintenance

- Documentation updates

**Verifying the Release**

In order to verify the release, you'll need to have `gpg` or `gpg2` installed on
your system. Once you've obtained a copy (and hopefully verified that as well),
you'll first need to import the keys that have signed this release if you
haven't done so already:
```
curl https://keybase.io/roasbeef/pgp_keys.asc | gpg --import
```

Once you have his PGP key you can verify the release (assuming
`manifest-v0.1.1-alpha.txt` and `manifest-v0.1.1-alpha.txt.sig` are in the
current directory) with:
```
gpg --verify manifest-v0.1.1-alpha.txt
```

You should see the following if the verification was successful:
```
gpg: assuming signed data in 'manifest-v0.1.1-alpha.txt'
gpg: Signature made Wed Apr 17 15:20:49 2019 PDT
gpg:                using RSA key F8037E70C12C7A263C032508CE58F7F8E20FD9A2
gpg: Good signature from "Olaoluwa Osuntokun <laolu32@gmail.com>" [ultimate]
```

That will verify the signature on the main manifest page which ensures integrity
and authenticity of the binaries you've downloaded locally. Next, depending on
your operating system you should then re-calculate the `sha256` sum of the
binary, and compare that with the following hashes (which are included in the
manifest file):
```
d3c3db1bed3ff0fb125514d9072d621492ff2b86f4a90200e93686052f18f5bd  loop-darwin-386-v0.1.1-alpha.tar.gz
ebf461a604b681d8a5c82bae14a0620a38a9f65604ca4f532aedb44ddc7c4ea0  loop-darwin-amd64-v0.1.1-alpha.tar.gz
bc83eb2eefb91054858c2a9164fdf245848f2f861d01526261e65dbd2d8237e0  loop-dragonfly-amd64-v0.1.1-alpha.tar.gz
ece2bb8e4c477627a4485f809e7f0d1c88893e01ca7e33363784c392875f00b6  loop-freebsd-386-v0.1.1-alpha.tar.gz
496941befd46331a4551ff9ff60fe4dc1a49434c1593960f3ed53a255aca0783  loop-freebsd-amd64-v0.1.1-alpha.tar.gz
6e21b065178d9616e78a63bacdca4401f8ed36a82dbdfb0d915099af29ae29da  loop-freebsd-arm-v0.1.1-alpha.tar.gz
3264e23ffe8994bd374c961f4973eaa6288e4dce0531967de85b23b342518824  loop-linux-386-v0.1.1-alpha.tar.gz
476f26083febb42fab15fa263048cf8bddcc86671aeaa98e1b74f5c422ebaee4  loop-linux-amd64-v0.1.1-alpha.tar.gz
9f13fb6cc9df51501e4c9560da96d96a9d0f385e600eb77fdbfdb97fb731a4a6  loop-linux-arm64-v0.1.1-alpha.tar.gz
53faaf225412c4f9dc16d437252f666243dba13f9135f07886ec019e5b109ebb  loop-linux-armv6-v0.1.1-alpha.tar.gz
4e8c04d1585476a8efb02805181dde80be0a1b45816d24f004924b6756298dd5  loop-linux-armv7-v0.1.1-alpha.tar.gz
a21b05379615a6d7d4de68c3edb2f5a341f23dd0f4895b646b5272e6b99e98d2  loop-linux-mips64-v0.1.1-alpha.tar.gz
047c2a9e62b477db7eb8c4f88a5bda68bcdd10bb396d4ba66ff34328a7b9e6d8  loop-linux-mips64le-v0.1.1-alpha.tar.gz
bd25a16e27fb3140be8ae62a8df1076a80ceac443420738d6261f0c375e60bb5  loop-linux-ppc64-v0.1.1-alpha.tar.gz
ef1a37d85c8ae1eb7fba34a66613344042f26867ab838017b3995ef4d7d8cd95  loop-netbsd-386-v0.1.1-alpha.tar.gz
1962c2b174bc401c5362ca1f7282e8129b6958dae08a1fd0248de6505d4b6c4c  loop-netbsd-amd64-v0.1.1-alpha.tar.gz
be27b8c84fac5c0e492aa7b395e3b344f8e0c7170525610e2df51238f8fea7de  loop-openbsd-386-v0.1.1-alpha.tar.gz
31ce199daf1af10f328af3b5d8b9cfcefc980eabffd6b91481ddf8a5d2b1e235  loop-openbsd-amd64-v0.1.1-alpha.tar.gz
406d5989ec935faf85dbdaccb88a659ba88ff35ca111b71bf769871f20139ccd  loop-source-v0.1.1-alpha.tar.gz
61395e480f7e1cecb9b945a7cad4a0b81186767474da5520dee48b6e89191f9b  loop-windows-386-v0.1.1-alpha.zip
01848825834fb4fc1c13841ffdfdc1b59b4f352d350d5b7ffbb3252885f0330d  loop-windows-amd64-v0.1.1-alpha.zip
2f0fcaca1479d04b49e85e0a3b7f70dd430cf675daaaa4b77104a83747bba3e0  vendor.tar.gz
```

One can use the `shasum -a 256 <file name here>` tool in order to re-compute the
`sha256` hash of the target binary for your operating system. The produced hash
should be compared with the hashes listed above and they should match *exactly*.

Finally, you can also verify the _tag_ itself with the following command:
```
git verify-tag v0.1.1-alpha
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
`loop-source-v0.1.1-alpha.tar.gz` are in the current directory:
```
tar -xvzf vendor.tar.gz
tar -xvzf loop-source-v0.1.1-alpha.tar.gz
GO111MODULE=on go install -v -mod=vendor -ldflags "-X github.com/lightninglabs/loop.Commit=v0.1.1-alpha" ./cmd/loop
GO111MODULE=on go install -v -mod=vendor -ldflags "-X github.com/lightninglabs/loop.Commit=v0.1.1-alpha" ./cmd/loopd
```

The `-mod=vendor` flag tells the `go build` command that it doesn't need to
fetch the dependencies, and instead, they're all enclosed in the local vendor
directory.

#### Contributors (Alphabetical Order)

- Alex Bosworth
- conscott
- Francisco Calderón
- Joost Jager
- Olaoluwa Osuntokun
