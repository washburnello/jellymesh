# Draft upstream change: go-fuse request timeout

**Status:** approved by the user (2026-09-30) and committed, ready to push.
go-fuse takes changes through Gerrit on GerritHub, not GitHub pull requests
(its CONTRIBUTING file). The commit is on branch `request-timeout` in
`~/Work/upstream/go-fuse`, against master at ef82fd5, with a Change-Id and
the unit test `TestInitRequestTimeout`. It waits for the user to link their
GitHub account at review.gerrithub.io; then it is pushed with:

    git push ssh://washburnello@review.gerrithub.io:29418/hanwen/go-fuse HEAD:refs/for/master

**Target:** github.com/hanwen/go-fuse (v2). Check the project's contribution
route first: it has historically taken changes through GerritHub as well as
GitHub.

## Proposed title

fuse: let a server request a kernel request timeout (FUSE_REQUEST_TIMEOUT)

## Proposed description

Since Linux 6.14 a FUSE server can ask the kernel, in its INIT reply, to
abort the connection when any request goes unanswered for a given number of
seconds (`FUSE_REQUEST_TIMEOUT`, `fuse_init_out.request_timeout`). This turns
a deadlocked or frozen server into failed requests for its callers, instead
of callers blocked indefinitely in the kernel.

go-fuse already defines `CAP_REQUEST_TIMEOUT` and `InitOut.RequestTimeout`,
but never sets them. This change:

- adds `MountOptions.RequestTimeout` (seconds, `uint16`; 0 keeps today's
  behaviour);
- in `doInit`, when the option is set and the kernel offers
  `CAP_REQUEST_TIMEOUT`, sets the flag and `InitOut.RequestTimeout`.

A kernel without the capability ignores the option, as before.

## Evidence

In a spike, a FUSE server paused with the cgroup freezer left a caller's
thread in uninterruptible sleep indefinitely. With `RequestTimeout: 20`, the
kernel aborted the connection after about 20 s, the waiting reads returned
errors, and no thread stayed blocked. The server then exited, was restarted,
and remounted. (Kernel 7.2; Jellymesh `lab/fusespike`, gate G1.)

## The change

The same as `go-fuse-request-timeout.patch` in this directory: 38 lines
across `fuse/api.go` and `fuse/opcode.go`.

## Before submitting, add

- A test in `fuse/` that mounts with `RequestTimeout` set and checks that
  the INIT reply carries the flag and value when the kernel offers the
  capability, and omits them when it does not. The capability can be
  simulated by constructing the INIT request directly, as other opcode tests
  do.
- A sentence in the `MountOptions` documentation noting that the kernel
  aborts the whole connection, not just the one request, so a server must
  be prepared to exit and remount.
