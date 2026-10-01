# go-fuse, patched

A copy of github.com/hanwen/go-fuse/v2 v2.11.0 (BSD-3-Clause, see LICENSE),
reduced to the packages Jellymesh builds (`fs`, `fuse`, `splice`, and their
internal dependencies; tests and other packages removed), with one change:

- `lab/fusespike/patches/go-fuse-request-timeout.patch`: adds
  `MountOptions.RequestTimeout`, which asks the kernel (6.14+) to abort the
  connection when a request goes unanswered that long
  (`FUSE_REQUEST_TIMEOUT`). The mount relies on it so that a frozen process
  fails Jellyfin's reads instead of hanging them (design-spec section 11,
  A-17).

`go.mod` replaces the module with this directory. To update: copy the same
packages from a newer release, reapply the patch, and rerun the FUSE
conformance tests. Once the change is in a release, delete this directory
and the replace. The upstream proposal is in
`lab/fusespike/patches/UPSTREAM.md`.
