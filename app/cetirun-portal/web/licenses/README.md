# Version-pinned license supplements

The npm tarballs that `supplements.json` lists do not contain their upstream
license file. The generator uses these upstream copies only for the listed
package versions. Before the generator includes a copy, it makes sure that the
SHA-256 of the copy is correct. The Fluent UI and Embla LICENSE files are
byte-for-byte copies. A final newline was added to the Fluent UI NOTICE. In the
Rollup license, the trailing whitespace and the final newline were normalized.

The Fluent UI source revision is the public npm `gitHead` for
`@fluentui/react-icons@2.0.343`. This value was verified on 2026-10-06. The
integrity of the npm tarball was verified, and the tarball contains no license
or notice files. For this package, the upstream LICENSE is unchanged. After a
final newline is added, the upstream NOTICE is the same as the stored copy.
Both source URLs refer to the commit of that version.

The Embla and Rollup source revisions are the public npm `gitHead` values for
`embla-carousel@8.6.0`, `embla-carousel-autoplay@8.6.0`,
`embla-carousel-fade@8.6.0`, and `@rollup/rollup-linux-x64-gnu@4.53.3`. These
values were verified on 2026-09-15. The attribution-file inventory of the Embla
root contains LICENSE. The inventory listed no other root NOTICE, COPYING,
AUTHORS, or CONTRIBUTORS file.

The Rollup native package is an optional dependency of `tabster@8.7.0`. It is
installed on Linux x64 but not on ARM64. Its supplement keeps the upstream
LICENSE.md, with the core license and the bundled dependency notices.
Before whitespace normalization, the SHA-256 of the original download is
`fa1bd040c5bdeefe65b3821cebf474f2733ce65df13089bd151dda1778e62fe8`.

When you upgrade one of these packages:

1. Examine the new npm tarball for its license and other notices.
2. If the tarball does not include them, make sure that its source repository
   and release revision are correct. Download the applicable upstream license
   and attribution files into a temporary directory. Examine each file to find
   if it applies to that package.
3. Update the version-specific entry, the source URL, the license copy, and the
   SHA-256 together. Do not use an old supplement for a new version without a
   review.
4. Run `npm run test:licenses`, `npm run licenses`, and `npm run build`.

The helper can prepare the metadata part of step 3 for an updated version of a
package that is already listed here. It does not change source files:

```bash
npm run licenses:check-updates -- --output /tmp/portal-license-update.patch
```

Use a new output path outside the web directory. This command uses the network.
If your host must use an HTTP(S) proxy, put `NODE_USE_ENV_PROXY=1` at the start
of the command.

The helper examines locked production versions. It also examines platform-only
optional packages. It reads public npm metadata and commit-pinned GitHub files.
It does not install or run package code.

The helper makes a proposal only if all of these conditions are true:

- The package identity and integrity match.
- The repository and the license declaration are the same.
- The attribution-file inventory is unchanged.
- The text is unchanged after CRLF and final-whitespace normalization.

Unknown packages, changed terms or notices, and unverified sources must have a
manual review. The helper does not examine tarball contents. Before you apply a
proposal, examine these contents. The helper does not approve a dependency.

Exit 0 means that known supplements already cover the locked versions. In this
case, the helper sends no network requests. Exit 1 means that a review is
necessary or that a request failed.

The helper creates a patch only for a verified update with unchanged text.
Portal CI uploads that patch as `portal-license-update`. It stays failed until
the PR includes a reviewed update. It never commits or pushes changes. The
normal build and the notice generator stay offline. Exact package versions are
necessary for both.

These copies keep the upstream copyright and terms. They do not give trademark
permission for project artwork. They do not cover other software in the images.
