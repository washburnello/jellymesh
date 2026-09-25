# Jellymesh Conformance Criteria

Status: Executable acceptance criteria
Date: 2026-09-25
Verify with: `./scripts/verify.sh`

## 1. Purpose

This document is the objective target the implementation is driven against. It
exists so that "is this done?" has an answer that does not depend on anyone's
recollection of a conversation.

Every criterion below is either bound to named Go tests, marked as requiring a
manual or lab procedure, or marked as a recorded assumption. `scripts/verify.sh`
extracts the test names, confirms each one still exists, and runs it. A
criterion whose test is renamed or deleted fails the run rather than quietly
becoming unverified, which is the usual way a document like this rots.

Sources: the release gates in [design-spec.md](design-spec.md) section 17, the
measured results in [phase-0-results.md](phase-0-results.md), and the open
findings in [plan-review.md](plan-review.md).

## 2. Status vocabulary

| Status | Meaning |
|---|---|
| `PASS` | Bound to tests that exist and pass. |
| `PARTIAL` | Partly proven; the gap is stated in the notes. |
| `PENDING` | Accepted requirement, not yet implemented. |
| `MANUAL` | Cannot be proven by unit test; needs a lab or pilot procedure. |
| `ASSUMED` | An open product decision resolved by a documented assumption in section 9, to be ratified or overridden. |

## 3. Node identity and transport

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-ID-1 | A node's identity is an Ed25519 key that persists across restart and is idempotent to load | `TestLoadOrCreateIsIdempotent` | PASS |
| C-ID-2 | The private key is owner-only, and a group- or world-readable key is refused rather than trusted | `TestKeyFileModeIsOwnerOnly`, `TestLoadRejectsGroupReadableKeyFile` | PASS |
| C-ID-3 | The fingerprint is derived from the public key, so identity survives certificate renewal | `TestFingerprintSurvivesCertificateRenewal`, `TestFingerprintIsStableAcrossReissuance` | PASS |
| C-ID-4 | A certificate whose key does not match the node's private key is rejected, never silently regenerated | `TestLoadRejectsMismatchedCertificate` | PASS |
| C-ID-5 | The node certificate is usable in both directions of a mutual-TLS handshake | `TestGeneratedCertificateCarriesBothExtKeyUsages` | PASS |
| C-TR-1 | Peers are authorized by key fingerprint, never by hostname or certificate chain | `TestHandshakeSucceedsWhenMutuallyTrusted`, `TestClientRejectsFingerprintMismatch` | PASS |
| C-TR-2 | An unknown or untrusted peer is refused | `TestServerRejectsUntrustedClient` | PASS |
| C-TR-3 | Revoking trust refuses subsequent handshakes | `TestRevokedFingerprintFailsSubsequentHandshake` | PASS |
| C-TR-4 | Two node identities may not share a fingerprint | `TestFingerprintCollisionBetweenTwoNodesIsRejected` | PASS |
| C-TR-5 | A successful handshake is not proof of authorization; the dial path must treat reachability and authorization separately | `TestClientHandshakeMayReturnNilDespiteServerRejection` | PASS |
| C-TR-6 | Peer authorization is durable and survives restart | `TestPeerRepositoryAsTrustStorePerformsRealHandshake` | PASS |

## 4. Trust, blocking, and fail-closed behaviour

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-BL-1 | A blocked peer is not trusted even when its trust flag is set | `TestBlockedPeerIsNotTrustedEvenWhenTrustedFlagIsTrue` | PASS |
| C-BL-2 | An empty fingerprint is never trusted | `TestEmptyFingerprintIsNeverTrusted` | PASS |
| C-BL-3 | The trust check fails closed on any database error | `TestIsTrustedFailsClosedWhenDatabaseIsClosed` | PASS |
| C-BL-4 | A block is a pairwise media cut that leaves both nodes group members, and unblocking preserves explicit opt-outs | `TestBlockOverridesPublishedLibrary`, `TestUnblockPreservesOptOut` | PASS |

## 5. Membership, roles, and publication

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-PO-1 | Every source library is private by default; nothing is published implicitly | `TestNonMemberCannotPublishIntoTheLivePool`, `TestPublishedLibraryIsAutoAccepted` | PASS |
| C-PO-2 | A destination auto-accepts published libraries and can opt out of any exact source library | `TestDestinationCanOptOutAndOptBackIn`, `TestOptOutRequiresPublishedLibrary` | PASS |
| C-PO-3 | An owner or any administrator may approve or deny a redeemed invitation; an ordinary member may not | `TestAnyMemberCanInviteButOnlyAdminsApprove`, `TestPromotedAdministratorCanApproveInvitations`, `TestAdministratorCanDenyInvitation` | PASS |
| C-PO-4 | An administrator cannot eject another administrator or the owner | `TestAdministratorCannotEjectAnotherAdministrator`, `TestOwnerCannotEjectItself`, `TestVerifiedRevocationCannotTargetOwner` | PASS |
| C-PO-5 | Ejection removes future participation and purges the member's publications; rejoining requires a fresh invitation and approval | `TestOwnerCanEjectAndRequireFreshAdmissionToRejoin`, `TestVerifiedRevocationPropagatesEjection` | PASS |
| C-PO-6 | A joining server must offer at least one non-empty library before admission, satisfiable before it is a member | `TestAdmissionRuleIsSatisfiedByStagedCandidates`, `TestMemberCannotStageCandidates` | PASS |
| C-PO-7 | Owner succession is never applied on a local timer; it requires attestations from a quorum of other members | `TestSuccessionRequiresAQuorumOfAttestations`, `TestSuccessionRejectsManufacturedQuorum`, `TestOnlyTheEligibleSuccessorMayClaim`, `TestClaimBeforeTheDeadlineIsRejected` | PASS |
| C-PO-8 | Membership, roles, publications, opt-outs, and invitations survive a restart, and behaviour after a reload matches behaviour before it | `TestMembershipRepositorySaveAndLoadRoundTrip`, `TestMembershipRepositorySaveUpdatesRatherThanDuplicating`, `TestMembershipRepositoryLoadUnknownGroupReturnsNotFound`, `TestMembershipRepositoryDelete`, `TestMembershipRepositoryListGroupIDs` | PASS |
| C-PO-9 | Group events are signed, tamper-evident across every field, domain-separated by kind, and replay-guarded by sequence | `TestMutatingAnySingleFieldInvalidatesTheSignature`, `TestVerifyWithDifferentNodesPublicKeyFailsWithInvalidSignature`, `TestIssuerMismatchWhenSignerLiesAboutItsOwnIdentity`, `TestAdmissionSignatureCannotBePresentedAsRevocation`, `TestSequenceGuardRejectsReplayAndStaleAcceptsMonotonicIncrease` | PASS |
| C-PO-10 | An event is applied only when its issuer is the owner or an administrator of that group; a correctly signed event from an ordinary member is refused | none yet | PENDING — `internal/events` verifies signatures but has no knowledge of roles, so any keypair currently produces a valid envelope. The authorization half must be enforced by the caller |
| C-PO-11 | The replay guard is seeded from durable state on restart, so an already-superseded sequence cannot be re-admitted by a freshly started node | none yet | PENDING — `SequenceGuard` is in-memory and starts empty; `MembershipSequence` is persisted and must seed it |
| C-PO-12 | A block is persisted for a peer that has never connected | none yet | PENDING — `peers.blocked` requires an existing peer row with a unique fingerprint, so blocking an unseen node id is currently not durable |

## 6. Durable state

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-ST-1 | State survives process restart without loss or duplication | `TestOpenIsIdempotent` | PASS |
| C-ST-2 | A multi-statement write is atomic, rolling back on both error and panic | `TestWithTxRollsBackOnError`, `TestWithTxRollsBackOnPanic`, `TestWithTxCommitsOnSuccess` | PASS |
| C-ST-3 | The retention expiry sweep uses an index rather than scanning every retained item | `TestRetentionExpiryUsesTheIndex`, `TestRetentionRepositoryExpireUsesTheIndex` | PASS |
| C-ST-4 | The database and its directory are owner-only | `TestDatabaseFileIsOwnerOnly` | PASS |
| C-ST-5 | A failed or incomplete sync never discards known-good state | `TestRecordFailurePreservesTheExistingCursor` | PASS |
| C-ST-6 | Provider health uses hysteresis; one failed request does not mark a provider offline | `TestSyncStateTransitionsFromUnknownThroughDegradedToUnavailable` | PASS |
| C-ST-7 | Encrypted backup and restore of keys, configuration, publications, opt-outs, and sync state | none yet | PENDING |

## 7. History and progress

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-HI-1 | Watched state, play count, and resume position are per local user and never shared between users | `TestLedgerIsolatesUsers`, `TestHistoryRepositoryWorksForUserIsolatesUsers` | PASS |
| C-HI-2 | Restoration fills only missing fields and never overwrites newer local progress | `TestMergePreservesLocalProgress`, `TestRestoreDoesNotLetTheLedgerBeatNewerLocalProgress` | PASS |
| C-HI-3 | A deliberate un-watch is never resurrected by the ledger | `TestMergeDoesNotResurrectDeliberateUnwatch` | PASS |
| C-HI-4 | Restoration actually applies when the local server holds no state for a work | `TestRestoreFillsGapsWhenLocalHasNothing`, `TestMergeStillRestoresWhenLocalHasNoRecord` | PASS |
| C-HI-5 | Generated content can be purged without losing per-user watched state, play count, or resume position | `TestRestoreWithNoPreservedRowReturnsLocalUnchanged`, `TestRetentionRepositoryRecordDeletionRoundTrip`, `TestReturningItemDiscardsRetention` | PARTIAL — the ledger and retention halves are proven; the purge path that drives them does not exist yet |
| C-HI-6 | Works are merged only on strong provider identity; title and year alone never merge | `TestStrongIdentityMatch`, `TestDifferentStrongIdentitiesDoNotMatch` | PARTIAL — the identity primitive is proven; it has no episode addressing, see plan-review D6 |

## 8. Not yet implemented

These are accepted requirements from the release gates with no implementation
behind them. They are listed so that the gap is explicit rather than implied.

| ID | Criterion | Status |
|---|---|---|
| C-PR-1 | A protected library is absent from remote catalog, search, artwork, subtitles, and playback, including by guessed identifier | PENDING |
| C-PR-2 | An opted-out library is absent from the same surfaces | PENDING |
| C-PR-3 | Generated artifacts and logs contain no credentials; a `.strm` holds a local relay reference and never a peer URL or bearer token | PENDING |
| C-PR-4 | Local Jellyfin library permissions are a browse boundary only and are not relied on for playback authorization | MANUAL — measured in phase-0-results.md section 8; the design must not assume otherwise |
| C-CA-1 | Catalog sync is incremental, paginated, and idempotent, rejecting stale revisions | PENDING |
| C-CA-2 | A confirmed tombstone removes the item immediately while retention preserves metadata for the grace period | PENDING |
| C-CA-3 | A library's root path set is recorded at publication; changing it pauses publication pending re-confirmation | PENDING — plan-review C1 |
| C-MA-1 | Generated artifacts are written atomically and never appear partially to Jellyfin | PENDING |
| C-MA-2 | Generated layout does not embed provider identifiers in directory names | PENDING — see assumption A-2 |
| C-MA-3 | External subtitles are materialized alongside generated references | PENDING — phase-0-results.md section 6 |
| C-PB-1 | The relay supports range requests, HEAD, cancellation, and backpressure without full-file buffering | PENDING — byte-exact pass-through measured in phase-0-results.md section 7 |
| C-PB-2 | An unavailable source fails playback cleanly without destructive catalog pruning | PENDING |
| C-PB-3 | A source enforces a bandwidth ceiling per destination | PENDING — see assumption A-1 |
| C-OP-1 | Audit events are recorded with secrets redacted | PENDING — the table exists, nothing writes to it |
| C-OP-2 | Key rotation and compromise recovery have a defined protocol | PENDING — design-spec.md section 8 records this as open |

## 9. Recorded assumptions

Open product decisions resolved here so implementation is not blocked. Each is
a decision that can be overridden; none is a finding.

**A-1. Source-side bandwidth ceiling (plan-review A4).**
Assumed: a source enforces a configurable per-destination bitrate ceiling,
defaulting to a value below a typical residential uplink. When a requested
media source exceeds the ceiling, the source Jellymesh Service requests a
transcoded rendition from its own Jellyfin rather than relaying the original.
Rationale: the measured relay path is a byte-exact pass-through, so without a
ceiling a single 4K remux saturates the source household's uplink. Placing the
transcode at the source is the only option that reduces bytes on the
constrained link. Cost: a double-transcode risk at the consumer, and the
`.strm` cannot negotiate per session, so the ceiling is per destination rather
than per playback.

**A-2. Provider identifiers in generated paths (phase-0-results.md section 7).**
Assumed: generated directories are namespaced by a Jellymesh-internal
identifier and carry no `[tmdbid-NNNNN]` token; provider identifiers travel in
NFO only. Rationale: two synthetic identifiers both resolved to unrelated real
titles, one of them adult, which means a stale or colliding identifier can pull
unrelated artwork into a family library.

**A-3. Music scope (phase-0-results.md section 5).**
Assumed: music is out of scope for v1. Rationale: federating music duplicated
the artist and album tree and NFO metadata did not repair it, and Jellyfin has
no alternate-version concept for audio to merge into. If music returns, the
assumed shape is a separate `Friends Music` library so the duplication is
contained rather than polluting the local artist list.

**A-4. Integrated versus separate libraries (plan-review A1, A2).**
Assumed: generated roots are added to the destination's existing libraries per
collection type, and Jellymesh actively drives and maintains version merges
through the Jellyfin merge API. Rationale: the extraction risk that argued
against integration was measured and disproven, and the merge API was validated
as a working mitigation. Cost: Jellymesh owns a merge graph it must split on
every tombstone.

## 10. Manual and pilot procedures

Not provable by unit test. These are the Phase 5 gates.

| ID | Criterion |
|---|---|
| M-1 | Stock Jellyfin Web, Android, Android TV, iOS/Swiftfin, and Roku TV can browse, play, seek, and report progress |
| M-2 | Public HTTPS works from an external network without Tailscale |
| M-3 | Backup, restore, key rotation, and rollback drills succeed |
| M-4 | A multi-home pilot survives reboots, outages, certificate renewal, and library changes |
| M-5 | A first join to a group of realistic size completes in an acceptable time, given the measured 4.72 items/second |
