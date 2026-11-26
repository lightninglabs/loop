# Loop Client Release Notes

#### New Features

* The `--postgres.password` flag now accepts a `@/path/to/file` syntax that
  reads the password from a file instead of taking it on the command line, so
  the secret is no longer exposed in process listings or shell history. A
  password that begins with a literal `@` is written as `@@password`.
  [Issue #1088](https://github.com/lightninglabs/loop/issues/1088)

#### Breaking Changes

* A `--postgres.password` value beginning with `@` is now read as the path to a
  file holding the password rather than as the password itself. Operators whose
  password starts with a literal `@` must double the prefix, as in
  `@@password`, before upgrading. Left unchanged, such a value either stops
  `loopd` from starting or, if a file happens to exist at the implied path,
  connects using that file's contents.
  [Issue #1088](https://github.com/lightninglabs/loop/issues/1088)

#### Bug Fixes

* Fix a deadlock in the static address loop-in, withdrawal and open channel
  managers. If the caller of a request stopped waiting before the manager
  answered, for example because the `loop` command was interrupted, the
  manager blocked on its response and served no further requests of that
  kind until `loopd` was restarted.

#### Maintenance

* Update the OpenTelemetry Go SDK from v1.45.0 to v1.47.0 in Loop and
  `looprpc`.

#### Contributors (Alphabetical Order)
