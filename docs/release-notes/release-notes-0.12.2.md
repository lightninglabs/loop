# Loop Client Release Notes

- **Release date:** 2021-04-29
- **Release page:**
  [v0.12.2-beta](https://github.com/lightninglabs/loop/releases/tag/v0.12.2-beta)
- **Previous release:** [v0.12.1-beta](release-notes-0.12.1.md)
- **Next release:** [v0.13.0-beta](release-notes-0.13.0.md)

#### New Features

- If the payment for an LSAT fails, it is now automatically re-tried.

#### Breaking Changes

#### Bug Fixes

 - Instead of just blocking for forever without any apparent reason if another
   Loop daemon process is already running, we now exit with an error after 5
   seconds if acquiring the unique lock on the Loop `bbolt` DB fails.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Andras Banki-Horvath
- Elle Mouton
- Oliver Gugger
- Yong Yu
