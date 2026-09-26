# Jellymesh Conformance Criteria

Status: Executable acceptance criteria
Date: 2026-09-26
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
| `DECIDED` | A product decision recorded in the design specification. What it depends on is tracked by the criteria named in its notes. |

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
| C-TR-5 | Under TLS 1.3 a client handshake reports success even when the server rejects the client certificate, and the rejection surfaces on first I/O (library behaviour, pinned so the caution on `ClientTLSConfig` cannot silently become false) | `TestClientHandshakeMayReturnNilDespiteServerRejection` | PASS |
| C-TR-8 | The dial path treats reachability and authorization separately, and does not consider a peer to have accepted this node until first I/O succeeds | none yet | PENDING — no dial path exists. Previously claimed PASS under C-TR-5 on the strength of a test that exercises only crypto/tls |
| C-TR-6 | Peer authorization is durable and survives restart | `TestPeerRepositoryAsTrustStorePerformsRealHandshake` | PASS |
| C-TR-7 | A known node's key cannot be replaced in place; a new key is a new peer that starts untrusted | `TestUpsertRefusesToChangeAKnownNodesFingerprint` | PASS |

## 4. Trust, blocking, and fail-closed behaviour

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-BL-1 | A blocked peer is not trusted even when its trust flag is set | `TestBlockedPeerIsNotTrustedEvenWhenTrustedFlagIsTrue` | PASS |
| C-BL-2 | An empty fingerprint is never trusted | `TestEmptyFingerprintIsNeverTrusted` | PASS |
| C-BL-3 | The trust check fails closed on any database error | `TestIsTrustedFailsClosedWhenDatabaseIsClosed` | PASS |
| C-BL-4 | A block is a pairwise media cut that leaves both nodes group members, and unblocking preserves explicit opt-outs | `TestBlockOverridesPublishedLibrary`, `TestUnblockPreservesOptOut` | PASS |
| C-BL-5 | A block, and a trust decision, change only through their explicit setters; no peer update or membership save lifts a block or re-trusts a peer as a side effect | `TestUpsertOfAKnownPeerDoesNotChangeTrustOrBlock`, `TestMembershipSaveDoesNotLiftABlock`, `TestMigrationCarriesExistingBlocksForward` | PASS |

## 5. Membership, roles, and publication

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-PO-1 | Every source library is private by default; nothing is published implicitly | `TestNonMemberCannotPublishIntoTheLivePool`, `TestPublishedLibraryIsAutoAccepted` | PASS |
| C-PO-2 | A destination auto-accepts published libraries and can opt out of any exact source library | `TestDestinationCanOptOutAndOptBackIn`, `TestOptOutRequiresPublishedLibrary` | PASS |
| C-PO-3 | An owner or any administrator may approve or deny a redeemed invitation; an ordinary member may not | `TestAnyMemberCanInviteButOnlyAdminsApprove`, `TestPromotedAdministratorCanApproveInvitations`, `TestAdministratorCanDenyInvitation` | PASS |
| C-PO-4 | An administrator cannot eject another administrator or the owner, whether locally or through a signed revocation | `TestAdministratorCannotEjectAnotherAdministrator`, `TestOwnerCannotEjectItself`, `TestVerifiedRevocationCannotTargetOwner`, `TestVerifiedRevocationEnforcesAdministratorProtection`, `TestSignedRevocationOfAnAdministratorByAnAdministratorIsRefused` | PASS |
| C-PO-5 | Ejection removes future participation and purges the member's publications; rejoining requires a fresh invitation and approval | `TestOwnerCanEjectAndRequireFreshAdmissionToRejoin`, `TestVerifiedRevocationPropagatesEjection` | PASS |
| C-PO-6 | A joining server must offer at least one non-empty library before admission, satisfiable before it is a member | `TestAdmissionRuleIsSatisfiedByStagedCandidates`, `TestMemberCannotStageCandidates` | PASS |
| C-PO-7 | Owner succession is never applied on a local timer; a claim is accepted only from the eligible successor, after the deadline, with a quorum of distinct, eligible attestors | `TestSuccessionRequiresAQuorumOfAttestations`, `TestSuccessionRejectsManufacturedQuorum`, `TestOnlyTheEligibleSuccessorMayClaim`, `TestClaimBeforeTheDeadlineIsRejected` | PASS |
| C-PO-14 | Each absence attestation is signed by its attestor and verified, so a claimant cannot author the quorum itself | `TestSuccessionRequiresSignedAttestationsFromAQuorum`, `TestEverySignedFieldIsCovered`, `TestAClaimMustContainEveryAttestedHead` | PARTIAL — enforced by `internal/grouplog`. The old path, `group.State.ClaimOwnership`, still accepts unsigned attestations until it is retired when the log is wired in |
| C-PO-15 | A former owner that returns after a successful succession claim is fenced: old-epoch events beyond the claim's base are refused, and members that applied them truncate and replay | `TestSuccessionFencesTheFormerOwner` | PASS |
| C-PO-8 | Membership, roles, publications, opt-outs, and invitations survive a restart, and behaviour after a reload matches behaviour before it | `TestMembershipRepositorySaveAndLoadRoundTrip`, `TestMembershipRepositorySaveUpdatesRatherThanDuplicating`, `TestMembershipRepositoryLoadUnknownGroupReturnsNotFound`, `TestMembershipRepositoryDelete`, `TestMembershipRepositoryListGroupIDs` | PASS |
| C-PO-9 | Group events are signed, tamper-evident across every field, domain-separated by kind, and replay-guarded by sequence | `TestMutatingAnySingleFieldInvalidatesTheSignature`, `TestVerifyWithDifferentNodesPublicKeyFailsWithInvalidSignature`, `TestIssuerMismatchWhenSignerLiesAboutItsOwnIdentity`, `TestAdmissionSignatureCannotBePresentedAsRevocation`, `TestSequenceGuardRejectsReplayAndStaleAcceptsMonotonicIncrease` | PASS |
| C-PO-10 | An event is applied only when its issuer is the owner or an administrator of that group, and the issuer's fingerprint matches the peer record; a correctly signed event from an ordinary member is refused | `TestCorrectlySignedEventFromOrdinaryMemberIsRefused`, `TestIssuerNameCannotBeBorrowedFromAnAdministrator`, `TestAdministratorMayIssueMembershipEvents`, `TestUnknownIssuerIsRefused` | PASS |
| C-PO-11 | The replay guard is seeded from durable state on restart, so an already-superseded sequence cannot be re-admitted by a freshly started node | `TestReplayGuardIsSeededFromDurableSequence`, `TestSeedNeverLowersTheHighWaterMark` | PASS |
| C-PO-12 | A block is persisted for a peer that has never connected, applies when it appears, and survives removal of its peer record | `TestBlockOfAnUnseenPeerIsDurableAndApplies` | PASS |
| C-PO-13 | An event refused after verification does not consume its sequence number, and a payload's sequence must match its signed envelope's | `TestRefusedEventDoesNotConsumeItsSequence`, `TestPayloadSequenceMustMatchEnvelopeSequence`, `TestSequenceGuardCheckDoesNotRecord` | PASS |
| C-PO-16 | A signed admission can be applied by every member, including ones that never saw the invitation, and binds the admitted node's key; a node ID never changes key and a key never moves to another node ID | `TestAdmissionAppliesFromTheLogAloneAndBindsTheKey`, `TestAdmissionNeverRebindsAKey`, `TestAnInvitationAdmitsOnce` | PASS |
| C-PO-17 | Group events have one order across all members: only the epoch's owner sequences, each event extends the previous by hash, and an event that arrives ahead of the head is held until the gap fills, never dropped | `TestOnlyTheOwnerSequences`, `TestAnEventMustExtendTheHeadByHash`, `TestOutOfOrderEventsAreHeldAndApplied`, `TestReceivingAnEventTwiceIsHarmless` | PASS |
| C-PO-21 | A node behind a peer's head fetches the missing range from that peer or any other member | none yet | PENDING — the replication protocol over mutual TLS |
| C-PO-18 | An administrator's decision reaches the log only as a proposal it signed, embedded in an owner-sequenced event; receivers verify both signatures and re-apply the role rules | `TestTheOwnerCannotFabricateAnAdministratorsDecision`, `TestReceiversReapplyTheRoleRules`, `TestAProposalAppliesOnce` | PASS |
| C-PO-19 | Two correctly signed events for the same epoch and sequence are detected as equivocation; the node stops applying past that point and keeps both as evidence, and no non-owner can trigger this | `TestEquivocationHaltsTheLogAndKeepsEvidence`, `TestAForgedConflictDoesNotHaltTheLog` | PASS |
| C-PO-20 | Group state is a pure function of the log: replaying the same log from genesis on any node yields identical state, and a tampered or foreign log is refused | `TestReplayingTheSameLogYieldsIdenticalState`, `TestReplayRefusesATamperedEvent`, `TestReplayAnchoredRefusesADifferentGroup`, `TestEventsSurviveTheWireEncoding` | PASS |
| C-TR-9 | Transport trust is derived from the replicated roster and local blocks, so admission and ejection change trust on every node with no separate trust write | none yet | PENDING — design: A-5. Replaces the `peers.trusted` write in `membership.Applier` |

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
| C-ST-8 | Stored timestamps compare as strings in chronological order, so SQL comparisons and ordering on time columns are correct | `TestStoredTimestampsSortChronologically` | PASS |

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
| C-PR-5 | The relay listener cannot be reached from outside the host, and requests to it are authorized locally rather than by bind address alone | none yet | PENDING — config accepts any relay address; plan-review.md notes loopback is insufficient once Jellyfin and Jellymesh are in separate network namespaces |
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
| C-OP-2 | Compromise recovery is by re-enrollment: a fresh key is a distinct peer, and readmission requires a new invitation and fresh approval | DECIDED — see design-spec.md section 8. The mechanism it relies on is covered by C-ID-3, C-TR-4, C-TR-7 and C-PO-5; what remains is C-OP-3 and an operator runbook |
| C-OP-3 | Ejecting or revoking a member also revokes trust at the transport layer, so a compromised key cannot complete a handshake | `TestEjectionRevokesTransportTrust`, `TestEjectionByANonAdministratorChangesNothing`, `TestAdministratorMayIssueMembershipEvents` | PASS |
| C-OP-4 | An operator runbook documents the compromise-recovery sequence | PENDING |

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

**A-5. Group state replication (C-PO-14 to C-PO-20, C-TR-9).**
Assumed: group state (owner, epoch, administrators, the roster bound to
fingerprints, ejections, group defaults) is derived by replaying an
append-only, hash-chained log that only the owner sequences. Administrators
act through signed proposals the owner embeds. Replication is pull-based with
gap repair. Succession opens a new epoch that fences the old owner. Transport
trust is derived from the roster. Publications, opt-outs, blocks, invitations,
and sync state stay node-local. Full design in design-spec.md section 8,
"Group state and event replication".
Rationale: one sequencer removes slot conflicts by construction rather than
resolving them. Hash chaining makes the log self-verifying from any relay.
Deriving trust from the roster removes the drift between membership and trust
that the review found.
Cost: while the owner's node is offline, administrator decisions queue rather
than apply, until the owner returns or succession completes. The alternative,
letting any administrator sequence, keeps working without the owner but needs
conflict resolution between concurrent administrators, which is the class of
bug this replaces.

## 10. Manual and pilot procedures

Not provable by unit test. These are the Phase 5 gates.

| ID | Criterion | Needs | Notes |
|---|---|---|---|
| M-1 | Stock Jellyfin Web, Android, Android TV, iOS/Swiftfin, and Roku TV can browse, play, seek, switch subtitle and audio tracks, resume, and report progress on a remote source | Real devices; Web needs only a browser | **The largest untested assumption in the project.** Phase 0 proved the server relays bytes and ranges correctly, but that was measured with `curl`; no Jellyfin client has yet played a federated item. The Web half is testable today against hand-authored `.strm` files, without any Jellymesh code |
| M-1a | A remote item plays, seeks, and resumes in Jellyfin Web | A browser session | Partial de-risk of M-1, available now |
| M-2 | Public HTTPS works from an external network without Tailscale | A domain, DNS, a port forward, a certificate | |
| M-2a | The node has a reachable public address at all | One minute of checking | Precondition for M-2. If the router's WAN address is inside `100.64.0.0/10` the ISP is using carrier-grade NAT, port forwarding cannot work, and the no-Tailscale goal fails for that node. Worth establishing before Phase 5 rather than during it |
| M-3 | Backup, restore, and rollback drills succeed, including compromise recovery by re-enrollment | Nothing external; waiting on C-ST-7 | Not hardware-blocked. Previously worded as a key-rotation drill, which no longer exists after the decision in design-spec.md section 8 |
| M-4 | A multi-home pilot survives reboots, outages, certificate renewal, and library changes | A second household willing to run alpha software, over weeks | The genuine long pole. Cannot be simulated: the failures it finds only appear over time |
| M-5 | A first join to a group of realistic size completes in an acceptable time | A real catalog size | Half-measured: the rate is 4.72 items/second (phase-0-results.md section 4); only the item count is missing |
