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
| C-TR-8 | The dial path treats reachability and authorization separately: a peer's refusal of this node, which under TLS 1.3 surfaces only on first I/O, is reported distinctly from an unreachable peer | `TestRefusalIsDistinguishedFromUnreachability` | PASS |
| C-TR-6 | Peer authorization is durable and survives restart | `TestTrustSurvivesRestart` | PASS |
| C-TR-7 | A known node's key cannot be replaced in place; a new key is a new peer that starts untrusted | `TestUpsertRefusesToChangeAKnownNodesFingerprint` | PASS |

## 4. Trust, blocking, and fail-closed behaviour

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-BL-1 | A blocked member is refused at the transport layer while remaining in the roster | `TestABlockedMemberIsRefused` | PASS |
| C-BL-2 | An empty fingerprint, or a key outside the roster, is never trusted | `TestTrustFailsClosed`, `TestMembersAreTrustedAndOutsidersAreNot` | PASS |
| C-BL-3 | The trust check fails closed on any database error, and a halted group trusts nobody | `TestTrustFailsClosed`, `TestAHaltedGroupTrustsNobody` | PASS |
| C-BL-4 | A block is a pairwise media cut that leaves both nodes group members, and unblocking preserves explicit opt-outs | `TestBlockOverridesPublishedLibrary`, `TestUnblockPreservesOptOut` | PASS |
| C-BL-5 | A block changes only through its explicit setter; no peer update or policy save lifts it as a side effect | `TestUpsertOfAKnownPeerDoesNotLiftABlock`, `TestPolicySaveDoesNotLiftABlock`, `TestMigrationCarriesExistingBlocksForward` | PASS |

## 5. Membership, roles, and publication

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-PO-1 | Every source library is private by default; nothing is published implicitly | `TestNonMemberCannotPublishIntoTheLivePool`, `TestPublishedLibraryIsAutoAccepted` | PASS |
| C-PO-2 | A destination auto-accepts published libraries and can opt out of any exact source library | `TestDestinationCanOptOutAndOptBackIn`, `TestOptOutRequiresPublishedLibrary` | PASS |
| C-PO-3 | An owner or any administrator may approve or deny a redeemed invitation; an ordinary member may not | `TestAnyMemberCanInviteButOnlyAdminsApprove`, `TestPromotedAdministratorCanApproveInvitations`, `TestAdministratorCanDenyInvitation` | PASS |
| C-PO-4 | An administrator cannot eject another administrator or the owner, the owner cannot leave, and receivers enforce this even on an owner-sequenced event | `TestReceiversReapplyTheRoleRules` | PASS |
| C-PO-5 | Ejection removes future participation and purges the member's publications; rejoining requires a fresh invitation and approval | `TestARemovedMembersPublicationsArePurged`, `TestTheGroupLogDrivesPublicationState`, `TestAnInvitationAdmitsOnce`, `TestAnOldEventReplayedAfterRestartChangesNothing` | PASS |
| C-PO-6 | A joining server must offer at least one non-empty library before admission, satisfiable before it is a member, and approval refuses without one | `TestAdmissionRuleIsSatisfiedByStagedCandidates`, `TestMemberCannotStageCandidates`, `TestReconcilePromotesAnAdmittedNodesCandidates` | PASS |
| C-PO-7 | Owner succession is never applied on a local timer: the local absence window only makes a claim eligible, and a claim is accepted only from the eligible successor, with a quorum of distinct members each attesting a full absence window | `TestTheAbsenceWindowOnlyMakesAClaimEligible`, `TestRepeatedUnavailabilityDoesNotPushTheDeadlineBack`, `TestSuccessionRequiresSignedAttestationsFromAQuorum` | PASS |
| C-PO-14 | Each absence attestation is signed by its attestor and verified, so a claimant cannot author the quorum itself | `TestSuccessionRequiresSignedAttestationsFromAQuorum`, `TestEverySignedFieldIsCovered`, `TestAClaimMustContainEveryAttestedHead` | PASS |
| C-PO-15 | A former owner that returns after a successful succession claim is fenced: old-epoch events beyond the claim's base are refused, and members that applied them truncate and replay | `TestSuccessionFencesTheFormerOwner` | PASS |
| C-PO-8 | A node's publications, candidates, opt-outs, blocks, invitations, and owner-absence window survive a restart, and behaviour after a reload matches behaviour before it; membership itself is the stored log (C-ST-9) | `TestPolicyRepositorySaveAndLoadRoundTrip`, `TestPolicyRepositorySaveReplacesRatherThanDuplicating`, `TestPolicyRepositoryLoadOfAnUnknownGroupIsEmpty`, `TestPolicyRepositoryDeleteKeepsBlocks`, `TestOwnerWatchSurvivesRestart`, `TestGroupLogRepositoryListGroupIDs` | PASS |
| C-PO-9 | Group events, proposals, and attestations are signed, tamper-evident across every field, and domain-separated; a proposal applies once | `TestEverySignedFieldIsCovered`, `TestSignaturesAreDomainSeparated`, `TestAProposalAppliesOnce`, `TestReplayRefusesATamperedEvent` | PASS |
| C-PO-10 | An event is applied only when its signer is the epoch's owner, verified with the key the roster holds, and its proposer holds the power it exercises; a correctly signed decision from an ordinary member is refused | `TestOnlyTheOwnerSequences`, `TestReceiversReapplyTheRoleRules`, `TestTheOwnerCannotFabricateAnAdministratorsDecision` | PASS |
| C-PO-11 | Replay protection survives restart: an event already in the stored log, presented again, changes nothing | `TestAnOldEventReplayedAfterRestartChangesNothing`, `TestReceivingAnEventTwiceIsHarmless`, `TestGroupLogRepositoryRoundTrip` | PASS |
| C-PO-12 | A block is persisted for a peer that has never connected, applies when it appears, and survives removal of its peer record | `TestBlockOfAnUnseenPeerIsDurableAndApplies` | PASS |
| C-PO-13 | A refused event leaves the log exactly as it was, so the legitimate event for that slot still applies | `TestReceiversReapplyTheRoleRules`, `TestAnEventMustExtendTheHeadByHash` | PASS |
| C-PO-16 | A signed admission can be applied by every member, including ones that never saw the invitation, and binds the admitted node's key; a node ID never changes key and a key never moves to another node ID | `TestAdmissionAppliesFromTheLogAloneAndBindsTheKey`, `TestAdmissionNeverRebindsAKey`, `TestAnInvitationAdmitsOnce` | PASS |
| C-PO-17 | Group events have one order across all members: only the epoch's owner sequences, each event extends the previous by hash, and an event that arrives ahead of the head is held until the gap fills, never dropped | `TestOnlyTheOwnerSequences`, `TestAnEventMustExtendTheHeadByHash`, `TestOutOfOrderEventsAreHeldAndApplied`, `TestReceivingAnEventTwiceIsHarmless` | PASS |
| C-PO-21 | A node behind a peer's head fetches the missing range from that peer or any other member, pages through a long log, converges on a succession it missed, and detects equivocation when heads differ at the same height; a member serving forged or out-of-order events cannot change the log | `TestAMemberCatchesUpFromAnyMember`, `TestSyncPagesThroughALongLog`, `TestAMinorityConvergesOnASuccessionBySync`, `TestSyncDetectsEquivocation`, `TestAPeerServingForgedEventsCannotCorruptTheLog` | PASS |
| C-PO-22 | A node serving several groups gives each group's log only to that group's members, and does not reveal to a non-member which groups it serves | `TestAMemberOfOneGroupCannotReadAnother`, `TestRefusalIsDistinguishedFromUnreachability` | PASS |
| C-PO-23 | A member's signed proposal reaches the log by submission to the owner's node, which sequences it under the same rules as any event and returns the event; a node that is not the owner says so, and any member may relay a proposal because its signature is what authorizes it | `TestAnAdministratorsProposalIsSequencedByTheOwner`, `TestTheOwnerRefusesAProposalTheRulesForbid`, `TestANodeThatIsNotTheOwnerRefusesToSequence`, `TestANonMemberCannotSubmit`, `TestAMemberOfOneGroupCannotReadAnother` | PASS |
| C-PO-18 | An administrator's decision reaches the log only as a proposal it signed, embedded in an owner-sequenced event; receivers verify both signatures and re-apply the role rules | `TestTheOwnerCannotFabricateAnAdministratorsDecision`, `TestReceiversReapplyTheRoleRules`, `TestAProposalAppliesOnce` | PASS |
| C-PO-19 | Two correctly signed events for the same epoch and sequence are detected as equivocation; the node stops applying past that point and keeps both as evidence, and no non-owner can trigger this | `TestEquivocationHaltsTheLogAndKeepsEvidence`, `TestAForgedConflictDoesNotHaltTheLog` | PASS |
| C-PO-20 | Group state is a pure function of the log: replaying the same log from genesis on any node yields identical state, and a tampered or foreign log is refused | `TestReplayingTheSameLogYieldsIdenticalState`, `TestReplayRefusesATamperedEvent`, `TestReplayAnchoredRefusesADifferentGroup`, `TestEventsSurviveTheWireEncoding` | PASS |
| C-TR-9 | Transport trust is derived from the replicated roster and local blocks, so admission and ejection change trust on every node with no separate trust write | `TestMembersAreTrustedAndOutsidersAreNot`, `TestEjectionRevokesTrustOnEveryNode` | PASS |

### Enrollment

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-EN-1 | A node joins end to end by short code or QR code: it redeems at the inviter, an administrator on another node approves, the owner sequences, and the node downloads a log anchored to the genesis and refuses, without storing it, a log that does not admit its own key | `TestANodeJoinsWithAShortCode`, `TestANodeJoinsWithAQRCode`, `TestJoinBeforeAdmissionFails`, `TestJoinRefusesALogThatDoesNotAdmitThisNode` | PASS |
| C-EN-2 | The admission binds the key the invitee presented in TLS when it redeemed; a request body cannot name a key, and an approver refuses a request whose key does not match its fingerprint | `TestTheRedeemingKeyIsTheOneAdmitted`, `TestRedemptionRecordsTheKeyTheInviteePresented` | PASS |
| C-EN-3 | The joining node authenticates the inviter before sending the secret: exactly by a QR code's fingerprint, or by a short code's 80-bit prefix; an impostor never receives the secret, and an inviter reporting a group other than the QR code's is refused | `TestAnImpostorNeverReceivesTheSecret`, `TestAQRInvitationForADifferentGroupIsRefused`, `TestShortCodeRoundTrip`, `TestQRRoundTrip`, `TestMalformedCodesAreRefused` | PASS |
| C-EN-4 | Wrong, used, and expired secrets are answered identically, redemption failures are rate-limited across all clients, and a secret redeems once | `TestFailedRedemptionsAreIndistinguishableAndRateLimited` | PASS |
| C-EN-5 | Only an owner or administrator can list or deny pending requests, and a denied invitee learns it and cannot download the log | `TestOnlyAdministratorsSeeAndDenyRequests` | PASS |
| C-EN-6 | On the federation listener a key outside every roster reaches only the enrollment routes; a member route without its own check is still private, and a public route outside the enrollment prefix is unreachable | `TestOnlyThePublicPrefixIsReachableByANonMember`, `TestANonMemberReachesOnlyEnrollment` | PASS |

### Catalog and source

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-SA-1 | The source adapter authenticates as a non-administrator service user, refuses an administrator account, sees only the libraries that user can see, pages through items, resolves an item's library from its ancestors, calls no administrator route, and re-authenticates when its token is refused | `TestTheServiceUserSeesOnlyItsLibraries`, `TestItemsPageAndResolveTheirLibrary`, `TestTheAdapterReauthenticatesWhenItsTokenIsRefused`, `TestAnAdministratorIsRefusedAsTheServiceUser` | PASS — the routes were confirmed against Jellyfin 10.11.11 (M-7) |
| C-SA-2 | The service user's password and access token never appear in the audit log or the adapter's errors, and an administrator API key is refused as configuration | `TestSecretsNeverAppearInErrors`, `TestCatalogsFlowBetweenMembersThroughTheDaemon`, `TestAnAdministratorKeyIsRefused` | PASS |
| C-PR-1 | A protected library is absent from remote catalog, search, artwork, subtitles, and playback, including by guessed identifier | `TestAProtectedLibraryIsNeverExposed`, `TestALibraryProtectedAfterPublicationIsWithdrawn`, `TestALibraryTheServiceUserCannotSeeCannotBePublished`, `TestTheMediaRouteAuthorizesAgainstLiveState`, `TestARemoteItemPlaysThroughTheWholeChain` | PASS — never published, listed, sent, materialized (so never in the destination's search), or served as media, subtitle, or image, including by guessed ID |
| C-PR-2 | An opted-out library is absent from the same surfaces | `TestAnOptedOutLibraryIsNeverSent`, `TestOptingOutAndBackIn`, `TestTheDestinationEnforcesItsOwnRules`, `TestARemoteItemPlaysThroughTheWholeChain` | PASS — not sent, dropped if sent, removed from the generated root, and refused by the relay at once, before the next pass |
| C-CA-1 | Catalog sync is incremental, paginated, and idempotent, rejecting stale revisions; each page and its cursor apply atomically, and a source cannot move the cursor backwards | `TestChangesAreIncrementalAndRevisioned`, `TestParentsComeBeforeChildren`, `TestADestinationSyncsASourceIncrementally`, `TestTheDestinationEnforcesItsOwnRules`, `TestASourceCannotMoveTheCursorBackwards` | PASS |
| C-CA-2 | A confirmed tombstone removes the item immediately while retention preserves its identity and metadata for the grace period; an item that returns is restored and no longer retained | `TestTombstonesRequireConfirmation`, `TestATransientFailureWithdrawsNothing`, `TestATombstoneRemovesAndRetains`, `TestUnpublishingRemovesTheLibraryDownstream` | PASS |
| C-CA-3 | A library's root path set is recorded at publication; an item outside it pauses and withdraws the publication until the operator confirms new roots | `TestPublicationRequiresEveryItemUnderTheRoots`, `TestAnItemOutsideTheRootsPausesThePublication` | PASS |
| C-CA-4 | A per-item request is authorized against the item's library as live Jellyfin state reports it now, so an item moved out of a published library is refused before any refresh | `TestAuthorizationUsesLiveLibraryMembership` | PASS |
| C-CA-5 | The source serves its catalog only to members it has not blocked, and never sends an opted-out library's metadata to that destination; the destination drops such items, and items of libraries the source does not list, even if a source sends them | `TestAnOptedOutLibraryIsNeverSent`, `TestOptingOutAndBackIn`, `TestTheDestinationEnforcesItsOwnRules`, `TestABlockedDestinationOrANonMemberGetsNothing`, `TestTheCatalogRoutesAuthorizeThemselves` | PASS |

## 6. Durable state

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-ST-1 | State survives process restart without loss or duplication | `TestOpenIsIdempotent` | PASS |
| C-ST-2 | A multi-statement write is atomic, rolling back on both error and panic | `TestWithTxRollsBackOnError`, `TestWithTxRollsBackOnPanic`, `TestWithTxCommitsOnSuccess` | PASS |
| C-ST-3 | The retention expiry sweep uses an index rather than scanning every retained item | `TestRetentionExpiryUsesTheIndex`, `TestRetentionRepositoryExpireUsesTheIndex` | PASS |
| C-ST-4 | The database and its directory are owner-only | `TestDatabaseFileIsOwnerOnly` | PASS |
| C-ST-5 | A failed or incomplete sync never discards known-good state | `TestRecordFailurePreservesTheExistingCursor`, `TestAFailedSyncKeepsKnownGoodState` | PASS |
| C-ST-6 | Provider health uses hysteresis; one failed request does not mark a provider offline | `TestSyncStateTransitionsFromUnknownThroughDegradedToUnavailable` | PASS |
| C-ST-7 | Encrypted backup and restore of the node key, certificate, and all durable state: the group log, publications, opt-outs, blocks, invitations, sync state, and audit log; configuration is supplied by the deployment's environment and backed up with it | `TestBackupRestoresTheNode` | PASS |
| C-ST-10 | A backup holds nothing in the clear; a wrong passphrase, any alteration including to the header, and a non-backup file are refused; a weak passphrase cannot make a backup; a restore never overwrites an existing node | `TestABackupHoldsNothingInTheClear`, `TestTamperingAndWrongPassphrasesAreRefused`, `TestRestoreNeverOverwritesANode` | PASS |
| C-ST-11 | A node restored from backup may not sequence or claim until it confirms it has caught up, because a restored owner missing events it already published would otherwise equivocate; the daemon releases the hold only on a heartbeat that reaches a majority of the other members with nothing newer, and a held owner's own decisions queue until then | `TestARestoredOwnerMustCatchUpBeforeSequencing`, `TestARestoredOwnerIsReleasedOnceItHasCaughtUp` | PASS |
| C-ST-8 | Stored timestamps compare as strings in chronological order, so SQL comparisons and ordering on time columns are correct | `TestStoredTimestampsSortChronologically` | PASS |
| C-ST-9 | The group log survives restart, is re-verified from genesis on every load so a tampered store is refused, persists a succession's truncation while keeping the removed events, and keeps a halted log halted | `TestGroupLogRepositoryRoundTrip`, `TestGroupLogRepositoryRefusesATamperedStore`, `TestGroupLogRepositoryPersistsSupersessionAndKeepsTheRemovedEvents`, `TestGroupLogRepositoryHaltSurvivesRestart` | PASS |

## 7. History and progress

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-HI-1 | Watched state, play count, and resume position are per local user and never shared between users | `TestLedgerIsolatesUsers`, `TestHistoryRepositoryWorksForUserIsolatesUsers` | PASS |
| C-HI-2 | Restoration fills only missing fields and never overwrites newer local progress | `TestMergePreservesLocalProgress`, `TestRestoreDoesNotLetTheLedgerBeatNewerLocalProgress` | PASS |
| C-HI-3 | A deliberate un-watch is never resurrected by the ledger | `TestMergeDoesNotResurrectDeliberateUnwatch` | PASS |
| C-HI-4 | Restoration actually applies when the local server holds no state for a work | `TestRestoreFillsGapsWhenLocalHasNothing`, `TestMergeStillRestoresWhenLocalHasNoRecord` | PASS |
| C-HI-5 | Generated content can be purged without losing per-user watched state, play count, or resume position | `TestAReturningWorkCarriesItsIdentifiers`, `TestAReturningWorkComesBackToItsFolder`, `TestAShowKeepsItsFolderAndIdentifiersWhenASourceAddsOne`, `TestPinsLastForTheRetentionPeriod`, `TestWorkPinsRoundTripTouchAndPrune`, M-10 | PASS — Jellyfin itself keeps a withdrawn item's per-user state and reattaches it to an item that returns under a shared identifier (M-10); Jellymesh keeps each work's folder and identifiers stable so that it does. Bounds in assumption A-14 |
| C-HI-6 | Works are merged only on strong provider identity; title and year alone never merge | `TestStrongIdentityMatch`, `TestDifferentStrongIdentitiesDoNotMatch`, `TestWorksJoinOnASharedIdentifier`, `TestWorksNeverJoinAcrossAConflict`, `TestOnlyStrongIdentifiersGroup`, `TestMoviesWithoutAStrongIdentityStaySeparate`, `TestSeriesWithoutASharedIdentityStayApart`, `TestAFilmAndAShowWithOneNumberStayApart`, `TestAPinHoldsItsWorkTogether` | PASS — items are one work when they share a TMDB, TVDB, or IMDb identifier and disagree on none, films and series separately; episodes are addressed by series work, season, and episode number (C-MA-6) |

## 8. Privacy, materialization, playback, and operations

Accepted requirements from the release gates, with what proves each so far.
A criterion with nothing behind it is listed anyway, so the gap is explicit
rather than implied.

| ID | Criterion | Verified by | Status |
|---|---|---|---|
| C-PR-3 | Generated artifacts and logs contain no credentials; a `.strm` holds a local relay reference and never a peer URL or bearer token | `TestGeneratedFilesHoldNoCredentialsOrPeerAddresses`, `TestSecretsNeverReachTheSink`, `TestAnUnreachableSourceFailsCleanly` | PASS |
| C-PR-4 | Local Jellyfin library permissions are a browse boundary only and are not relied on for playback authorization | none yet | MANUAL — measured in phase-0-results.md section 8; the design must not assume otherwise |
| C-PR-5 | The relay listener cannot be reached from outside the host, and requests to it are authorized locally rather than by bind address alone | `TestOnlyAllowedClientsMayUseTheRelay`, `TestUnknownReferencesAndPolicyRefusalsReachNoSource`, `TestRelayConfiguration`, `TestARemoteItemPlaysThroughTheWholeChain` | PASS — allow-listed client addresses, issued and unrevoked references, and the destination's policy on every request; the .strm URL can carry no credential |
| C-MA-1 | Generated artifacts are written atomically and never appear partially to Jellyfin: each file is renamed into place from a dot-prefixed temporary name, metadata before the .strm, and an unchanged pass writes and fetches nothing | `TestWritesAreAtomicAndOrdered` | PASS |
| C-MA-2 | Generated layout does not embed provider identifiers in directory or file names; they live in NFO only | `TestAMovieFromTwoSourcesSharesAFolder`, `TestSanitize` | PASS |
| C-MA-3 | External subtitles are materialized alongside generated references, forced ones marked, and a failed fetch is retried on the next pass | `TestSubtitlesAndImagesAreServedOnlyAsListed`, `TestSubtitlesAreCopiedBesideTheReference` | PASS |
| C-PB-1 | The relay supports range requests, HEAD, cancellation, and backpressure without full-file buffering, at the source's media route and at the destination's local relay | `TestAMemberStreamsWithRangesAndHead`, `TestADisconnectStopsTheReadFromJellyfin`, `TestRangesAndHeadPassThrough`, `TestTheRelayStreamsAndCancelsUpstream` | PASS |
| C-PB-2 | An unavailable source fails playback cleanly, as a 502 with a plain reason, without destructive catalog pruning | `TestAnUnreachableSourceFailsCleanly`, `TestAFailedSyncKeepsKnownGoodState` | PASS |
| C-PB-3 | A source enforces a bandwidth ceiling per destination, shared by all of that destination's streams | `TestTheCeilingBoundsADestinationsStreamsTogether` | PASS |
| C-MA-4 | Removing a generated item revokes its relay reference first, deletes only its own files and only inside the generated root, drops a folder with nothing left to play, and does not disturb other versions | `TestRemovalRevokesAndStaysInsideTheRoot`, `TestEpisodesAreLaidOutBySeriesAndSeason` | PASS |
| C-MA-5 | A movie with a strong identity from several sources is materialized in one folder with one source-named file per source, and a movie without one never shares a folder | `TestAMovieFromTwoSourcesSharesAFolder`, `TestMoviesWithoutAStrongIdentityStaySeparate`, `TestVersionLabelsAreDistinct`, `TestAFilmKnownByDifferentIdentifiersIsOneWork`, `TestTwoCopiesFromOneSourceAreTwoVersions` | PASS — the NFO carries every identifier of the work, and two copies from one source are labelled apart |
| C-MA-7 | Generated metadata names each item's sources as `From <source>` tags (a film every source it has a version from, a show the sources its episodes play from, an episode the one it plays from) and carries the source's studios, tagline, and community and critic ratings | `TestNFOsNameTheirSourcesAndCarrySourceMetadata`, `TestChangesCarrySourceMetadata`, `TestRemovalRevokesAndStaysInsideTheRoot` | PASS — Jellyfin 10.11.11 reads each element from a generated NFO (M-8) and the adapter receives them from a real source (M-7) |
| C-MA-8 | A film returning to a path it held, from its source or another, and an episode moving to another source, keep the .strm's reference, rebound to the new item, so Jellyfin plays it before its next scan; while the film is gone the reference resolves to nothing, and a path's reference is kept for reuse at most 7 days | `TestAReturningFilmKeepsItsReference`, `TestAnEpisodeMovingSourceKeepsItsReference`, `TestRetiredReferencesAreReusedOnceAndExpire` | PASS — found by the M-3 recovery drill (#61). A reference names an item without authorizing anyone, and the relay checks the destination's policy on every request, so reuse grants nothing new. Rerun on the LAN test 2026-09-29: after the recovery drill re-enrolled walnut, cedar's Jellyfin streamed its film exactly before any rescan |
| C-MA-6 | A series with a shared strong identity from several sources is one show holding every source's episodes; each episode, addressed by series work, season, and episode number, has one file from one source, which keeps its place until it goes; unnumbered episodes are never grouped | `TestASeriesFromTwoSourcesIsOneShow`, `TestAnEpisodeKeepsItsSourceUntilItGoes`, `TestUnnumberedEpisodesKeepAFileEach`, `TestSeriesWithoutASharedIdentityStayApart` | PASS — one file per episode because Jellyfin shows no episode versions (M-10, A-13) |
| C-PB-4 | A stream above a destination's bandwidth ceiling is served as a source-side transcode rather than throttled | none yet | PENDING — the second half of assumption A-1 |
| C-PB-5 | Jellyfin's repeated probe of a remote item on every PlaybackInfo does not repeatedly cross the source's uplink: the relay answers from a per-revision head cache, confirms each hit with a bodiless HEAD so the source still authorizes every request, and stitches exactly past the head | `TestRepeatedProbesAreServedFromTheHead`, `TestRangesInsideTheHeadAndHeadRequestsStayLocal`, `TestARangePastTheHeadIsStitchedExactly`, `TestTheHeadIsTiedToOneRevisionOfOneFile`, `TestACachedHeadIsNotServedOnceTheSourceRefuses`, `TestTheCacheIsBoundedAndRemovable` | PASS |
| C-NT-1 | Joining is approved only once the approver's node has reached the joiner's advertised address and found the joiner's key there | `TestAdvertisedAddressMustAnswerWithTheJoinersKey`, `TestAnUnreachableJoinerIsNotApproved` | PASS — a pinned TLS handshake to the advertised address before the admission is proposed; an empty address, an unreachable one, and one where a different key answers are each refused by name, and a refused approval changes nothing |
| C-NT-2 | A node learns its outside UDP address from STUN on the socket its QUIC transport uses, and tells a mapping that varies by destination from one that does not | `TestDiscoveryLearnsTheAskingSocketsOutsideAddress`, `TestDiscoveryTellsWhetherTheMappingVariesByDestination`, `TestDiscoveryIgnoresPacketsItDidNotAskFor`, `TestDiscoveryFailsCleanlyWhenNoServerAnswers`, `TestDiscoveryAndQUICShareOneSocket` | PASS — discovery runs on the direct-path endpoint's own socket while QUIC listens on it (`quic-go` hands non-QUIC datagrams back), learns that socket's outside address, detects a mapping that varies by destination, and refuses stray, forged, and unasked answers |
| C-NT-3 | Direct-path offers are exchanged only over the mutually authenticated connection to a member, name only that pair, and expire; an offer from a non-member, for another pair, or replayed late is refused | `TestOffersAreBoundToThePairAndUsedOnce`, `TestOffersNameOnlyAFewUsableAddresses`, `TestOffersAreExchangedOverTheMemberRoute`, `TestAnswersAreCheckedLikeOffers`, `TestMediaTakesTheDirectPathAndFallsBackToTCP` | PASS — an offer must name its authenticated caller and this node, live at most 20 s (with a minute of clock tolerance), be used once, and name at most 8 ordinary unicast candidates, and the caller checks the answer the same way. In the daemon the route sits on the federation listener's member routes, the caller comes from the TLS key and the roster, and a blocked member's offer is refused |
| C-NT-4 | Two nodes behind endpoint-independent NATs establish a QUIC connection by hole punching, authenticated by the same pinned node keys, and a mismatched key is refused | `TestADirectConnectionUsesThePinnedKeys`, `TestADirectConnectionRefusesTheWrongKey`, `TestPunchingIsBounded`, `TestThePunchStartsTogetherDespiteClockDifference`, M-13 | PASS — the pinned keys, refusal at the handshake, bounded punching, and a start time shared across clocks that differ by up to 40 s are proven by tests; traversal of two NATs, and of a double NAT like cedar's, is proven by the NAT lab (M-13), 7 of 7 runs. Wiring into the daemon comes with C-NT-5 and C-NT-6 |
| C-NT-5 | Media over the direct path keeps every relay guarantee: ranges, HEAD, cancellation, the per-destination ceiling, the head cache, and the source's authorization of each request | `TestMediaRequestsKeepTheirMeaningOverADirectPath`, `TestCancellingADirectStreamStopsTheSource`, `TestMediaTakesTheDirectPathAndFallsBackToTCP`, `TestTheCeilingHoldsOverTheDirectPath` | PASS — media runs as the same HTTP requests to the same federation handlers, as HTTP/3 over the direct connection, with the caller's pinned key in `request.TLS`. Through two daemons: whole files, ranges, and the file's end arrive exactly through the relay and its head cache, a source's block holds, and the source's ceiling bounds a direct transfer as it does one over TCP |
| C-NT-6 | When punching fails, media falls back to the advertised address without the viewer seeing an error, and the node reports which path each peer uses | `TestADirectStreamThatBreaksContinuesOverTCP`, `TestMediaTakesTheDirectPathAndFallsBackToTCP`, `TestAVaryingMappingIsNotOffered`, `TestABusyDirectPortIsReported` | PASS — a direct-path port another program holds turns direct paths off with the reason shown in `jellymesh status`, and media uses TCP; playback never waits for a path: media uses TCP until a direct path opens in the background, and after a failed attempt the next is at least 2 minutes later. A request that fails on the direct path is retried over TCP. A body that breaks part-way is continued over TCP from the exact byte, once, and a TCP answer starting elsewhere is not spliced in. A node whose NAT varies its mapping does not offer that address. `jellymesh status` shows each peer's path, the outside address, and the NAT type |
| C-NT-7 | The direct-path UDP surface limits abuse: a connection attempt from an address no accepted offer named is refused before any cryptography, a member may not offer more than once per 10 s, and path outcomes are audited with identifiers only, never addresses | `TestAConnectionFromAnUnofferedAddressIsRefusedEarly`, `TestADirectConnectionRefusesTheWrongKey`, `TestAMemberMayNotOfferTooOften`, `TestMediaTakesTheDirectPathAndFallsBackToTCP` | PASS — each layer is tested alone: an unoffered address with the right key is refused before the handshake, and an offered address with the wrong key is refused in it. The threat review is in design-spec section 8 |
| C-FS-1 | In FUSE presentation the materializer writes, for each remote film, a descriptor holding only its reference, size, bitrate, and modification time, in the layout, grouping, and naming of `.strm` presentation, named for the film the mount shows so sidecars match it; the time stays put while the film is unchanged; a film whose size neither its catalog nor its source gives is reported and not shown; removal revokes and removes it with its sidecars | `TestFilesPresentationWritesDescriptorsInTheSameLayout`, `TestADescriptorKeepsItsTimeUntilTheFilmChanges`, `TestAFilmWithoutASizeIsSizedAtItsSource`, `TestAFilmWithNoWayToSizeItIsNotShown`, `TestRemovingAFilmRemovesItsDescriptor`, `TestChangesDescribeTheMediaFile` | PASS — sources now send each item's file size, bitrate, and extension (never its path); Jellyfin 10.11.11 reports the size exactly (2026-09-30, walnut's LAN test: 1469241147 bytes, as on disk) |
| C-FS-2 | The mount shows the generated root read-only: each descriptor as its film, at the film's size and time; descriptors and dot-files hidden; changes on disk shown without a remount; film contents readable only by the allowed uids, while anyone sees names and sizes and reads metadata | `TestTheMountShowsFilmsAsFiles`, `TestChangesOnDiskAreShown`, `TestOnlyAllowedUsersReadFilms` | PASS — on a real FUSE mount (kernel 7.2); the uid check runs at open and again on every read |
| C-FS-3 | Film bytes come from the daemon's read service in whole 1 MiB chunks: exact across chunk boundaries and at the end, read-ahead only once reading is sequential (doubling to 16 chunks), a source file of another size an error rather than wrong bytes, and a read that misses its deadline (10 s) an I/O error that never holds the service | `TestReadsReturnTheFilmsBytes`, `TestReadAheadStartsOnlyWhenReadingIsSequential`, `TestAFileOfAnotherSizeIsAnError`, `TestAStuckReadFailsWithinTheDeadline`, `TestAnAbandonedReadEndsWithItsRequest` | PASS |
| C-FS-4 | Every read through the mount is authorized as a relay request is: the reference must be issued and unrevoked and the destination's policy must allow it, on every read, cached or not; a chunk fetched from the source is served for at most a minute, which bounds how long a refusal at the source goes unseen | `TestReadsStopAtOnceWhenRefusedOrRevoked`, `TestAHeldChunkIsServedOnlyForItsLife`, `TestARemoteFilmPlaysThroughTheMount` | PASS |
| C-FS-5 | A film whose read failed is retried every 10 s, whether the read service saw the failure or the mount reported it; once readable its modification time moves forward an hour so Jellyfin probes it again; the list survives a restart, and a withdrawn film is dropped from it | `TestAFailedFilmIsHealedOnceReadable`, `TestMarkChangedMovesTheTimeAnHour`, `TestAStuckReadFailsWithinTheDeadline`, `TestARemoteFilmPlaysThroughTheMount` | PASS — that Jellyfin reprobes after an hour's move and not a second's is the spike's G2-E; rerun against the build under #73 |
| C-FS-6 | Switching a node's presentation moves every item to its new form at the next pass, removing the old one, in either direction | `TestSwitchingPresentationMovesEveryItem` | PASS — the materializer's part. That Jellyfin reattaches watch history across the move is M-14 (#72) |
| C-FS-7 | A mount that stops answering cannot hang Jellyfin: the kernel aborts it after the request timeout, through the patched go-fuse; the watchdog finds it and the process exits to be restarted; and start detaches a dead mount and takes its place | `TestTheKernelAbortsAFrozenMount`, `TestTheWatchdogFindsAFrozenMount`, `TestStartReplacesADeadMount` | PASS — with a 1 s timeout a frozen mount's reader got "software caused connection abort" after 15 s, the kernel's check interval |
| C-FS-8 | A remote film plays through the whole chain in FUSE presentation: published by its source, written as a descriptor, shown by the mount beside its subtitle and poster, and read through the read socket as the source's exact bytes; opting out stops reads at once, before any catalog pass | `TestARemoteFilmPlaysThroughTheMount` | PASS |
| C-FS-9 | A node chooses its presentation by a self-test at setup and after each update, and the mount refuses to run on a kernel without FUSE request timeouts | none yet | PENDING — #70. The refusal is in `mount.Start` (`ErrNoRequestTimeout`), but no host here lacks request timeouts to show it |
| C-OP-1 | Audit events are recorded with secrets redacted: detail outside an allow-list of identifiers and outcomes is replaced, registered secret material is scrubbed from every field, and enrollment, group log changes including equivocation, and block decisions are audited without the invitation secret appearing in any encoding | `TestSecretsNeverReachTheSink`, `TestEnrollmentIsAuditedWithoutSecrets`, `TestGroupChangesAreAudited`, `TestBlockDecisionsAreAudited`, `TestAuditRepositoryRoundTrip` | PASS |
| C-OP-2 | Compromise recovery is by re-enrollment: a fresh key is a distinct peer, and readmission requires a new invitation and fresh approval | none yet | DECIDED — see design-spec.md section 8. The mechanism it relies on is covered by C-ID-3, C-TR-4, C-TR-7 and C-PO-5; C-OP-3 is in place and the runbook is C-OP-4 |
| C-OP-3 | Ejecting a member also revokes its transport trust on every node that applies the ejection, so a compromised key cannot complete a handshake; an ejection by a non-administrator changes nothing | `TestEjectionRevokesTrustOnEveryNode`, `TestReceiversReapplyTheRoleRules` | PASS |
| C-OP-4 | An operator runbook documents the compromise-recovery sequence | none yet | DECIDED — [runbook-compromise-recovery.md](runbook-compromise-recovery.md), using the commands the CLI provides; rehearsing it is part of the M-3 drill |
| C-OP-5 | Nodes form and operate a group through the daemon's admin API alone: founding, joining by short code, promotion, approval by an administrator that is not the owner, a proposal queued while the owner is unreachable and delivered when it returns, ejection revoking trust, and state surviving a restart; a node founds or joins at most one group | `TestAGroupFormsAndOperatesThroughTheDaemon` | PASS |
| C-OP-6 | The admin API is served only on loopback and only to a caller holding the owner-only admin token | `TestTheAdminListenerMustBeLoopback`, `TestTheAdminAPIRequiresTheToken`, `TestTheAdminTokenIsOwnerOnly` | PASS |
| C-OP-7 | Catalogs flow between members through the daemon: publication with declared roots, a protected library refused, a join offering the joiner's own publications and refused when it publishes nothing, both members' catalogs held by the other, opting out (with nothing sent) and back in, a block in either direction, and ejection removing the ejected member's items | `TestCatalogsFlowBetweenMembersThroughTheDaemon` | PASS |
| C-OP-8 | A remote item plays through the whole chain: the source publishes it, the destination materializes a .strm, subtitle, and poster, and the destination's relay returns the source's bytes for the .strm's URL, ranges included; opting out (at once), a block from either side, and protection keep it from playing, and a blocked source's items are withdrawn | `TestARemoteItemPlaysThroughTheWholeChain` | PASS |
| C-PO-24 | Succession runs over the wire: each heartbeat updates the node's owner-absence watch; once the window elapses, members send signed attestations to the eligible successor, whose node keeps only attestations that verify against its log and claims once it holds a quorum; every member follows the new epoch and the returning former owner can no longer act as owner | `TestSuccessionRunsOverTheWire`, `TestTheSuccessorKeepsOnlyValidAttestations` | PASS |

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
Superseded in part by A-11 and A-15: versions are grouped by file naming,
and the merge API is not used, since a non-administrator cannot call it.
Assumed: generated roots are added to the destination's existing libraries per
collection type, and Jellymesh actively drives and maintains version merges
through the Jellyfin merge API. Rationale: the extraction risk that argued
against integration was measured and disproven, and the merge API was validated
as a working mitigation. Cost: Jellymesh owns a merge graph it must split on
every tombstone.

**A-6. Invitation short code (C-EN-3).**
Assumed: a short code is 29 Crockford base32 characters, entered with the
inviter's address. It carries a 64-bit one-time secret and the first 80 bits
of the inviter's key fingerprint. The joining node pins the TLS connection to
that prefix before it sends the secret, and it learns the genesis hash over
the pinned connection. The QR form carries the full fingerprint and the
genesis hash.
Rationale: a short code has to authenticate the inviter, or anyone on the
path could accept the secret and impersonate the inviter. Matching 80 bits of
fingerprint needs a second-preimage search far beyond anyone attacking a
household invitation. The 64-bit secret cannot be guessed online against the
redemption rate limit within an invitation's lifetime.
Cost: 29 characters is longer than a typical pairing code. A PAKE such as
CPace could make it shorter, at the cost of a protocol and a dependency, and
is the natural revisit if the length proves a problem in the pilot.

**A-7. One group per node (C-OP-5).**
Assumed: in this release a node founds or joins at most one group. The
daemon refuses a second.
Rationale: routing enrollment for several groups on one listener needs a
redemption directory across groups with a shared rate limit, and blocks are
already node-wide rather than per group. Nothing in the product decisions
calls for a household server in two friend groups at once.
Cost: a household that belongs to two separate circles of friends needs a
second node. The replication layer already authorizes each request against
the group it names, so lifting the limit later is a daemon change, not a
protocol change.

**A-8. A non-administrator Jellyfin service user (plan-review B3).**
Assumed: a node reads its own Jellyfin as a dedicated non-administrator user
that can see exactly the libraries it may publish, never with an
administrator API key. A protected set of library IDs is refused even if the
service user can see it.
Rationale: the guarantee that `Family Movies` is never exposed then rests on
Jellyfin's own permissions ("the credential cannot see it") before it rests
on Jellymesh's checks. Holding an administrator key would make a Jellymesh
compromise a full Jellyfin compromise.
Cost: one extra setup step per server, since the operator creates the user
and grants its libraries. Jellyfin's raw stream route ignores library
permissions (phase-0-results.md section 8), so per-item requests still
re-resolve the item's library against live state.

**A-9. Declared root paths (plan-review C1).**
Assumed: the operator declares each published library's root paths when
publishing. Every refresh checks every item's path against them, withholds
anything outside, and pauses the publication until it is confirmed again.
Rationale: listing a library's configured paths needs an administrator,
which A-8 rules out. Item paths, however, are visible to an ordinary user
(phase-0-results.md section 8). Declaring the roots makes the operator state
what they intend to share, and turns a path added later into a visible pause
instead of a silent leak.
Cost: the operator has to know their library folders. A wrong declaration
fails safe, because publication is refused or paused rather than widened.

**A-10. Relay authorization (C-PR-5).**
Assumed: the local relay serves only allow-listed client addresses (loopback
by default, plus the address Jellyfin connects from in a container
deployment), and only for random 128-bit references the materializer issued
and has not revoked. It re-checks the destination's policy on every request,
and the source re-checks membership, blocks, and the item's live library.
Rationale: Jellyfin forwards no credential when it opens a `.strm`, and a
local user can read the `.strm` URL. So the URL cannot be the credential, and
the network position of the caller has to be.
Cost: a deployment where Jellyfin reaches the relay through a NAT address has
to add that address to `JELLYMESH_RELAY_ALLOWED_CLIENTS`.

**A-11. Generated layout (C-MA-2, C-MA-5).**
Assumed: the layout in design-spec section 9, "Materialization and relay".
Folder names carry a Jellymesh-internal ID and never a provider ID. A movie
with a strong identity shares one folder across sources, with files named
`<folder> - <source>` so Jellyfin shows one item with one version per source.
Rationale: confirmed on 10.11.11 in M-8. It also delivers source-labelled
versions without the administrator-only merge API that A-4 anticipated.
Cost: merging a remote version with a local copy of the same film still
needs that API; see A-15. Series are grouped across sources too, with one
file per episode (A-13).

**A-12. Detection by Jellyfin's scheduled scan.**
Assumed: Jellymesh sends Jellyfin a best-effort change notice and relies on
its scheduled library scan, whose interval the operator sets.
Rationale: a non-administrator cannot trigger a refresh, and the real-time
monitor did not fire in the lab (M-8). Holding an administrator credential
to force a refresh would undo A-8.
Cost: a new remote item appears only at the next scan. A withdrawn one stops
being playable at once but stays listed until then.

**A-13. One source per episode (C-MA-6).**
Assumed: each episode of a show is materialized from one source. The source
already playing it keeps it while it remains consumable; otherwise the first
by source node and item ID is chosen. The file name carries no source, so a
change of source keeps the episode's path.
Rationale: Jellyfin 10.11.11 shows episode files from two sources as two
episodes, whether named `<episode> - <source>`, in a folder per episode, or
`<episode> [<source>]` (M-10). Two items for one episode would also share a
key under which Jellyfin retains state (A-14). This departs from design-spec
section 10's rule that Jellymesh never prefers one source's version, which
holds for films only.
Cost: a viewer cannot pick an episode's source, and a source that is a
member but unreachable keeps its episodes until it withdraws them; failing
over on health is Phase 6 work.

**A-14. History is Jellyfin's, kept stable by Jellymesh (C-HI-5).**
Assumed: Jellyfin's own per-user state is the history. When an item is
removed, Jellyfin 10.11 detaches its users' state and reattaches it to an
item that appears with a shared identifier: watched state, play count,
resume position, and favourites came back for films returning under another
folder and source, IMDb-only films, films whose identifiers grew or shrank,
and episodes, across clean-up tasks and a restart (M-10). Jellymesh's part is
to keep what Jellyfin keys on stable. A work keeps the folder it was first
materialized under, and a show keeps exactly the identifiers it was first
written with, for as long as it is materialized and 90 days after
(`PinRetention`). A pin takes part in grouping, so it holds together items
that no longer share an identifier among themselves.
Rationale: the design-spec's ledger would need every local user's state,
which a non-administrator service user cannot read or write (A-8), and M-10
showed Jellyfin already does the job. The ledger package (`internal/history`,
C-HI-1 to C-HI-4) stays as a tested primitive but is not in the path.
Cost, and the bounds of C-HI-5: Jellyfin keys episode state under the
series' TVDB identifier before its TMDB one, so a show first written without
TVDB keeps being written without it. State is not restored for a work that
returns after its pin has expired, after an administrator has run Jellyfin's
unscheduled user-data clean-up task (which deletes state detached for over 90
days), or with none of its earlier identifiers. When two works that were
separate are found to be one, the older folder is kept and state held under
the younger one's identifiers is not carried over.

**A-15. Remote copies are not merged with local copies (C-HI-5).**
Assumed: a remote film that the destination also holds locally is a second
item beside the local one, with its own per-user state.
Rationale: merging needs `POST /Videos/MergeVersions`, which answers 403 to
a non-administrator (M-10), and holding an administrator credential would
undo A-8. Jellyfin's automatic series grouping hid the second series card in
the lab but its episodes were not listed under the remaining card, so it is
not relied on either.
Cost: a film held locally and remotely shows twice, and watching one does
not mark the other.

**A-16. Reachable addresses, direct media paths (C-NT-1 to C-NT-6).**
Assumed, decided with the user 2026-09-29: every node must advertise an
address other members can reach over TCP, by a router port forward or by
Tailscale Funnel in raw-TCP mode. Joining checks it. The advertised address
carries control traffic and arranges connections. Media prefers a direct QUIC
path over a hole-punched UDP route, arranged through that address, and falls
back to the address when punching fails. Design in design-spec.md section 8,
"Reachability and direct media paths".
Rationale: operators run servers at home, so asking for a port forward or a
free Tailscale account is a fair bar. Funnel alone would route every film
through Tailscale's servers under unpublished bandwidth limits; punching
keeps media home to home. Every member already knows every other member's
address from the roster, so no central matchmaker is needed.
Cost: a new UDP transport (QUIC) and a dependency on public STUN servers. A
pair whose routers both allocate a new port per destination streams through
the advertised address, and for a Funnel node that means Tailscale's limits.

**A-17. Remote films presented as files through FUSE (#60).**
Decided with the user 2026-09-30: remote films appear to Jellyfin as
ordinary files served by a small FUSE mount process, not as `.strm`
references, on hosts that pass a self-test. Other hosts keep `.strm`. The
mode is chosen once per node, at setup and after updates, and never
switched per incident. Remote films live in their own libraries with
trickplay and chapter extraction off. Design in design-spec section 11,
"Presentation through a virtual filesystem".
Rationale: some apps play a `.strm` URL themselves; the Jellyfin Roku app
always does, so remote films failed on it. Through FUSE every app streams
through Jellyfin: the same Roku played, seeked, resumed, and switched
subtitles (#60 G4). Stability outranks compatibility, so FUSE was adopted
only after it passed every stability gate:
- Jellyfin never hung under source, reader, or mount failures (G1).
- The library never shrank (G2).
- Nothing but probes crossed the network (G3).
- Everything recovered by itself (G5).
- It felt as fast as `.strm` (G6).
- It survived a 791-cycle soak (G7).
Cost: Linux hosts with `/dev/fuse`, a privileged mount container, shared
mount propagation, and a kernel with FUSE request timeouts. About 1 MB per
film is read at scan time from sources' uplinks. A patch to go-fuse is
carried until it is upstream. Remote films are in separate libraries rather
than the main ones.

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
| M-1 | Stock Jellyfin Web, Android, Android TV, iOS/Swiftfin, and Roku TV can browse, play, seek, switch subtitle and audio tracks, resume, and report progress on a remote source | Real devices; Web needs only a browser | **The largest untested assumption in the project.** Phase 0 proved the server relays bytes and ranges correctly, but that was measured with `curl`. 2026-09-29, against the LAN test (M-11) on cedar's Jellyfin: **iPhone and iPad played perfectly**, through Jellyfin, as the transcode logs show. **Roku failed** on a Hisense 50R6+ Roku TV running Jellyfin Roku 3.2.3: it browsed fine but every play stopped at 0 ms. The cause is in the app. `LoadVideoContentTask.bs` hands a remote source's `MediaSource.Path` (the `.strm` URL) to the TV's own player, both when the server offers direct play and in its `isHTTPStream()` branch, so the TV fetched `http://127.0.0.1:18190/...` from itself. No server setting changes that branch. Options and the FUSE spike are in #60. **The same Roku played films presented through the FUSE spike** (#60 gate G4, 2026-09-29), as did iPhone and iPad, with progress, resume, and subtitle switching working on the Roku. Android and Android TV are deferred: no devices are available yet |
| M-1a | A remote item plays, seeks, and resumes in Jellyfin Web | A browser session | Partial de-risk of M-1, available now |
| M-2 | A node is reachable from an external network at its advertised address, by a router port forward or by Tailscale Funnel in raw-TCP mode, with pinned keys intact end to end | A second network; Funnel or a port forward | Performed 2026-09-29 for cedar through Funnel: `tailscale funnel --tcp 10000 tcp://192.168.87.247:18443`. From walnut, over the internet to Funnel's public edge (199.38.181.54) with cedar's name in the TLS handshake (`lab/natlab/natpunch reach -connect`), cedar's pinned key answered in 0.38 s and a wrong key was refused. Funnel routes by that name, so a node must advertise its Funnel name, never the IP. Not yet tried from a genuinely separate network (#56) |
| M-2a | The node has a reachable public address at all | One minute of checking | Checked 2026-09-28 for cedar: public IPv4 198.98.94.5, but the Google Wifi's WAN is 10.160.0.75 behind a Calix gateway at 10.160.0.1 (double NAT, not carrier-grade NAT). No global IPv6. `tailscale netcheck` shows endpoint-independent mapping (`MappingVariesByDestIP: false`), which hole punching needs |
| M-12 | Two homes on different networks stream a film over a hole-punched UDP path, arranged through one node's Funnel address, and fall back to the advertised address when punching is prevented | A second network | Pending the direct-path work (A-16) |
| M-13 | A direct QUIC path opens through emulated home NATs using the real `internal/natpath` code and signalling over a forwarded port, and fails, detected, through NATs that allocate a port per destination | Docker | Performed 2026-09-29 with `lab/natlab/run.sh`: isolated networks, Linux routers doing MASQUERADE ("easy") or MASQUERADE `--random-fully` ("hard"), and 15 ms each way on the routers' outside links. Easy to easy and double NAT to easy connected and moved 32 MiB, checksum-verified, at 35 to 45 MB/s in 7 of 7 runs. Later the same day, easy to easy connected in 5 of 5 runs at 5 ms each way and 5 of 5 at 50 ms. Hard to easy and hard to hard found no path, and each hard side reported its mapping as varying. The run found that Linux conntrack NAT, as in many home routers, records a peer's packet that arrives before its own node has sent anything, then moves that node's outgoing packets to another port. So both sides must start within less than the one-way delay, which led to the start time carried in the answer, and to timing the clock estimate over the request alone and not the connection setup |
| M-14 | A node moved from `.strm` to FUSE presentation keeps its users' watched state, play counts, resume positions, and favourites on every remote film and episode, which reappear under their new paths at the next scan | Docker, or the LAN test | Pending (#72). The materializer's part is C-FS-6; history should reattach by provider identifier as M-10 found for other moves (A-14) |
| M-3 | Backup, restore, and rollback drills succeed, including compromise recovery by re-enrollment | The LAN test | Performed 2026-09-29 with `lab/lantest/drill.sh` on the running LAN test, walnut under test and cedar as owner. **Backup**: an encrypted backup of the running walnut node, restored into a fresh volume, came back as the same node (ID, key, group, members), and cedar's Jellyfin streamed its bytes exactly. Rolling back to the original volume did the same. **Compromise recovery**, by the runbook: after cedar ejected walnut, cedar could no longer play from it. Walnut rebuilt with a new key published again, rejoined by a fresh short code with the new fingerprint shown for out-of-band confirmation, and served again. The old key, brought back, reached no peer. Found: cedar's Jellyfin kept each re-materialized `.strm`'s revoked reference until its next scan, so the returning source's films failed until then. Fixed by C-MA-8 (#61); rerun on that build, cedar played the rebuilt node's film exactly before any rescan, and the drill now starts from whichever volume walnut is on |
| M-4 | A multi-home pilot survives reboots, outages, certificate renewal, and library changes | A second household willing to run alpha software, over weeks | The genuine long pole. Cannot be simulated: the failures it finds only appear over time |
| M-5 | A first join to a group of realistic size completes in an acceptable time | A real catalog size | Measured 2026-09-29 at cedar's size (255 films, 35 series, 150 seasons, 2,994 episodes, read from cedar's Jellyfin): `JELLYMESH_SCALE=1 go test ./internal/daemon -run FirstJoinAtCedarScale`. Two daemons in process on loopback, so network delay is excluded. The source built its catalog in 0.8 s, and the destination synced and materialized 3,249 playable items (6,528 files) in 0.9 s. The long pole is the destination Jellyfin's scan. At the rate measured in Phase 0 (4.72 items/s) that is about 11 to 12 minutes. With the FUSE presentation (#60) add a probe of about 1 MB per item, about 3.2 GB, which takes about 22 minutes at the default 20 Mbit/s ceiling, from the source's uplink, once. Not measured: a real scan at this size |
| M-6 | The container image builds, starts as a non-root user with a writable data volume, reports healthy, and is administered through `docker exec` | Docker | Performed 2026-09-26 against `deploy/docker-compose.yml`'s image: built, healthy within seconds, a group founded and reported through `/jellymesh status` inside the container. Repeat after changes to the Dockerfile |
| M-7 | The source adapter's routes and fields behave against real Jellyfin 10.11.11 as the fake assumes: `/UserViews`, `/Items` with `ParentId` paging and the `Path` and `Etag` fields, `/Items/{id}/Ancestors`, an administrator route refused, and token refusal leading to re-authentication, for a non-administrator user | Docker | Performed 2026-09-27 with `lab/adaptercheck/run-throwaway.sh`: a fresh container, metadata fetchers off, two granted libraries and one withheld. All 13 checks passed; the withheld library was invisible. Rerun 2026-09-28 with a source NFO giving a studio, tagline, and ratings, which the adapter received: all 14 checks passed. Repeat on every Jellyfin upgrade |
| M-8 | Jellyfin 10.11.11 treats generated artifacts as the layout assumes, and a non-administrator can use the source media routes | Docker | Performed 2026-09-28 with `lab/materializecheck/run-throwaway.sh`. The title and year come from the NFO. A `[jmid-…]` folder tag stays out of the name. Only the legacy `<tmdbid>` NFO element sets the provider ID. Dot-prefixed temporary files are ignored. Two `<folder> - <source>.strm` files make one item with versions labelled by source. A sidecar `.srt` attaches to its version. A non-administrator gets 403 from `/Library/Refresh` and `/Items/{library}/Refresh`, 204 but no effect from `/Library/Media/Updated`, and 206 with ranges from `/Videos/{id}/stream?static=true`. HEAD returns length and type, and the subtitle route returns 200. The real-time monitor did not detect new content within 150 s, even a real `.mkv` on btrfs after a restart. Rerun 2026-09-28: the generated NFO's `<tag>`, `<studio>`, `<tagline>`, `<rating>`, and `<criticrating>` all took effect (C-MA-7), and a removed folder stayed listed until a scan, as A-12 expects. Repeat on every Jellyfin upgrade |
| M-9 | The whole playback chain works against real Jellyfin 10.11.11 in the container deployment: a source Jellyfin, a destination Jellyfin, and two Jellymesh nodes from the current image on one Docker network | Docker | Performed 2026-09-28 with `lab/playcheck/run-throwaway.sh`. walnut joined cedar's group by short code and materialized cedar's film with its subtitle. After a scan, the destination Jellyfin showed the NFO's title, year, overview, and provider ID, and the external subtitle. The version's path was the local relay URL, and `PlaybackInfo` named no peer. Jellyfin streamed the source's exact bytes through the relay for a 100-byte range and for the whole 3,000,000-byte file. The run found and fixed a repeated year in folder names. Rerun 2026-09-28 after Phase 4's work grouping and pins: passed with exact bytes, and the film kept the folder ID it had before, so films already materialized do not move on upgrade. Repeat on every Jellyfin upgrade |
| M-10 | Jellyfin 10.11.11 keys per-user state as A-13, A-14, and A-15 assume | Docker | Performed 2026-09-28 with `lab/identitycheck/run-throwaway.sh`: a fresh container, metadata fetchers off, each library holding a local and a generated root. Watched state, play count, resume position, and favourites survived withdrawal, a scan, the clean-up tasks, and a restart, returning under a new folder and source for films (TMDB, IMDb-only, identifiers grown or shrunk) and for episodes; a film returning with a different identifier started fresh. Episodes lost their state when the returning show gained a TVDB identifier, and kept it when it gained TMDB. A local copy and a generated copy of one work kept separate state. Episode files from two sources became two episodes under all three namings tried. Two watched generated copies withdrawn in one scan did not abort it. A non-administrator got 403 from `/Videos/MergeVersions`. A first run that gave two libraries the same identifiers lost one library's episode state, which is why one work must be one item. Repeat on every Jellyfin upgrade |
| M-11 | Two real servers federate over the LAN: walnut publishes from its own Jellyfin, and cedar's production Jellyfin 10.11.11 lists and streams the films through Jellymesh | Two hosts on one LAN | Server side performed 2026-09-28 with `lab/lantest/up.sh` (teardown `down.sh`, then `restore.sh`). cedar founded the group, since a founder need not publish; walnut joined by short code over the LAN and published three films. Its federation port bypassed walnut's `ufw`, as Docker's published ports do. cedar materialized them into a dedicated "Jellymesh Test" library under Jellyfin's existing mount, so Jellyfin needed no restart, with relay and admin on loopback ports clear of cedar's services. cedar's Jellyfin showed the NFO titles, years, TMDB and IMDb identifiers, and a `From walnut` tag. Each version's path was the loopback relay. A user's play count on a film moved out of cedar's own library reattached to its Jellymesh copy (A-14 on a production server). A 100-byte range from the middle of a 1.4 GB film streamed through cedar's Jellyfin in 0.12 s and matched walnut's file exactly. The user then played the films through cedar's Jellyfin and found playback smooth and responsive (the client was not recorded, so M-1 still needs a per-client pass). Teardown found that removing the library before Jellyfin had scanned orphaned its items, and that a scan skips a completely empty root. `down.sh` now keeps a placeholder, scans, and removes the library only once the items are gone. After `restore.sh` the originals were back and verified, and the play counts from before the test and from the test itself were on them. Jellyfin's clean-up task had rewritten the collection file while the films were away, and the backup restored it |
