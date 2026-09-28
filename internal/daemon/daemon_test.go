package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellymesh/internal/backup"
	"jellymesh/internal/config"
	"jellymesh/internal/enrollment"
	"jellymesh/internal/jellyfin"
	"jellymesh/internal/jellyfin/jellyfintest"
	"jellymesh/internal/policy"
	"jellymesh/internal/store"
)

type testDaemon struct {
	t       *testing.T
	name    string
	dataDir string
	address string
	node    *Node
	admin   *httptest.Server
	token   string
	stop    context.CancelFunc
	done    chan error
}

func startDaemon(t *testing.T, name string, dataDir string, address string) *testDaemon {
	t.Helper()
	return startDaemonWith(t, name, dataDir, address, nil)
}

// startDaemonWith starts a daemon after configure has adjusted its config,
// for example to give it a Jellyfin service user.
func startDaemonWith(t *testing.T, name string, dataDir string, address string, configure func(*config.Config)) *testDaemon {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listen %s: %v", name, err)
	}
	d := &testDaemon{t: t, name: name, dataDir: dataDir, address: listener.Addr().String()}
	cfg := config.Config{
		NodeName:                name,
		PublicHostname:          d.address,
		FederationListenAddress: d.address,
		DataDirectory:           dataDir,
		NodeKeyPath:             filepath.Join(dataDir, "node.key"),
		NodeCertPath:            filepath.Join(dataDir, "node.crt"),
		GeneratedRootPath:       filepath.Join(dataDir, "generated"),
		RelayURL:                "http://127.0.0.1:8090",
		RelayAllowedClients:     "127.0.0.0/8,::1",
	}
	if configure != nil {
		configure(&cfg)
	}
	if d.node, err = Open(context.Background(), cfg, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	if d.token, err = LoadOrCreateAdminToken(filepath.Join(dataDir, AdminTokenName)); err != nil {
		t.Fatalf("token: %v", err)
	}
	d.admin = httptest.NewServer(d.node.AdminHandler(d.token))
	ctx, cancel := context.WithCancel(context.Background())
	d.stop, d.done = cancel, make(chan error, 1)
	go func() { d.done <- d.node.Run(ctx, listener, time.Hour) }()
	t.Cleanup(d.shutdown)
	return d
}

func (d *testDaemon) shutdown() {
	if d.stop == nil {
		return
	}
	d.stop()
	<-d.done
	d.admin.Close()
	d.node.Close()
	d.stop = nil
}

func (d *testDaemon) call(method string, path string, body any, into any) int {
	d.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request, _ := http.NewRequest(method, d.admin.URL+path, reader)
	request.Header.Set("Authorization", "Bearer "+d.token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		d.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if into != nil && response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, into); err != nil {
			d.t.Fatalf("decode %s: %v: %s", path, err, data)
		}
	}
	if response.StatusCode != http.StatusOK && into != nil {
		d.t.Logf("%s %s: %d %s", method, path, response.StatusCode, data)
	}
	return response.StatusCode
}

func (d *testDaemon) must(method string, path string, body any, into any) {
	d.t.Helper()
	if status := d.call(method, path, body, into); status != http.StatusOK {
		d.t.Fatalf("%s %s on %s: status %d", method, path, d.name, status)
	}
}

func (d *testDaemon) status() Status {
	var status Status
	d.must(http.MethodGet, "/admin/v1/status", nil, &status)
	return status
}

func (d *testDaemon) sync() SyncResult {
	var result SyncResult
	d.must(http.MethodPost, "/admin/v1/sync", nil, &result)
	return result
}

// join takes a node from invitation to membership through the admin API: the
// inviter invites, the joiner redeems, approver approves, the joiner joins.
func join(t *testing.T, inviter *testDaemon, joiner *testDaemon, approver *testDaemon) {
	t.Helper()
	joinOffering(t, inviter, joiner, approver, []policy.Library{{ID: joiner.name + "-movies", Name: "Movies", CollectionType: "movies"}})
}

// joinOffering is join with the libraries the joiner offers; nil means its
// own publications.
func joinOffering(t *testing.T, inviter *testDaemon, joiner *testDaemon, approver *testDaemon, libraries []policy.Library) {
	t.Helper()
	var invitation InviteResponse
	inviter.must(http.MethodPost, "/admin/v1/invitations", InviteRequest{ValidForSeconds: 3600}, &invitation)
	var pending PendingJoin
	joiner.must(http.MethodPost, "/admin/v1/join/redeem", RedeemAdminRequest{
		ShortCode: invitation.ShortCode, Address: invitation.Address, Libraries: libraries,
	}, &pending)
	var waiting PendingJoin
	joiner.must(http.MethodPost, "/admin/v1/join/complete", pending, &waiting)
	if waiting.Status != string(policy.InvitationAwaitingApproval) {
		t.Fatalf("%s before approval: %q", joiner.name, waiting.Status)
	}

	var requests []enrollment.Request
	approver.must(http.MethodGet, "/admin/v1/requests", nil, &requests)
	if len(requests) != 1 {
		t.Fatalf("%s sees %d requests, want 1", approver.name, len(requests))
	}
	var outcome Outcome
	approver.must(http.MethodPost, "/admin/v1/requests/approve", DecisionRequest{
		InviterID: requests[0].InviterID, InvitationID: requests[0].InvitationID,
	}, &outcome)
	if !outcome.Sequenced {
		t.Fatalf("approval by %s was not sequenced: %+v", approver.name, outcome)
	}
	inviter.sync()

	var joined PendingJoin
	joiner.must(http.MethodPost, "/admin/v1/join/complete", pending, &joined)
	if joined.Status != string(policy.InvitationAdmitted) {
		t.Fatalf("%s after approval: %q", joiner.name, joined.Status)
	}
}

// C-OP-5: three nodes form a group through their admin APIs alone: founding,
// joining by short code, promotion, approval by an administrator that is not
// the owner, a proposal queued while the owner is down and delivered when it
// returns, and state that survives a restart.
func TestAGroupFormsAndOperatesThroughTheDaemon(t *testing.T) {
	cedar := startDaemon(t, "cedar", t.TempDir(), "127.0.0.1:0")
	walnut := startDaemon(t, "walnut", t.TempDir(), "127.0.0.1:0")
	juniper := startDaemon(t, "juniper", t.TempDir(), "127.0.0.1:0")

	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	if status := cedar.call(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-2"}, nil); status != http.StatusConflict {
		t.Fatalf("founding a second group: status %d, want 409 (assumption A-7)", status)
	}

	join(t, cedar, walnut, cedar)
	cedar.must(http.MethodPost, "/admin/v1/members/"+walnut.node.NodeID()+"/promote", nil, nil)
	walnut.sync()
	if walnutStatus := walnut.status(); walnutStatus.Group == nil || len(walnutStatus.Group.Members) != 2 {
		t.Fatalf("walnut's view: %+v", walnutStatus.Group)
	}

	// walnut, an administrator but not the owner, approves juniper.
	join(t, cedar, juniper, walnut)
	walnut.sync()
	if cedar.status().Group.Sequence != walnut.status().Group.Sequence || juniper.status().Group.Sequence != cedar.status().Group.Sequence {
		t.Fatal("all three nodes should hold the same log")
	}

	// The owner goes down. walnut's ejection of juniper queues.
	cedarDir, cedarAddress := cedar.dataDir, cedar.address
	cedar.shutdown()
	var queued Outcome
	walnut.must(http.MethodPost, "/admin/v1/members/"+juniper.node.NodeID()+"/eject", nil, &queued)
	if !queued.Queued || walnut.status().Group.Queued != 1 {
		t.Fatalf("with the owner down the ejection should queue: %+v", queued)
	}

	// The owner returns at its address, from its own disk, and the queue
	// drains on walnut's next heartbeat.
	cedar = startDaemon(t, "cedar", cedarDir, cedarAddress)
	walnut.sync()
	if walnut.status().Group.Queued != 0 {
		t.Fatal("the queued ejection should have been delivered")
	}
	cedarStatus := cedar.status()
	for _, member := range cedarStatus.Group.Members {
		if member.NodeID == juniper.node.NodeID() {
			t.Fatal("the owner should have sequenced the ejection")
		}
	}
	if cedar.node.server.IsTrusted(juniper.node.Identity().Fingerprint()) {
		t.Fatal("the ejected node must lose transport trust")
	}
}

// The admin API refuses requests without the token.
func TestTheAdminAPIRequiresTheToken(t *testing.T) {
	cedar := startDaemon(t, "cedar", t.TempDir(), "127.0.0.1:0")
	response, err := http.Get(cedar.admin.URL + "/admin/v1/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d without a token, want 401", response.StatusCode)
	}
	cedar.token = "wrong"
	if status := cedar.call(http.MethodGet, "/admin/v1/status", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("status %d with a wrong token, want 401", status)
	}
	health, err := http.Get(cedar.admin.URL + "/healthz")
	if err != nil || health.StatusCode != http.StatusOK {
		t.Fatalf("health check: %v, %v", health, err)
	}
	health.Body.Close()
}

func TestTheAdminTokenIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), AdminTokenName)
	first, err := LoadOrCreateAdminToken(path)
	if err != nil || len(first) != 64 {
		t.Fatalf("create: %q, %v", first, err)
	}
	if second, _ := LoadOrCreateAdminToken(path); second != first {
		t.Fatal("the token must persist")
	}
	if err := chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := LoadOrCreateAdminToken(path); err == nil {
		t.Fatal("a readable token file must be refused")
	}
}

func chmod(path string, mode os.FileMode) error { return os.Chmod(path, mode) }

// C-ST-11 end to end: an owner restored from an older backup is held back from
// sequencing, catches up on its first heartbeat, and is released on the next,
// once a majority of the other members have nothing newer to offer.
func TestARestoredOwnerIsReleasedOnceItHasCaughtUp(t *testing.T) {
	cedar := startDaemon(t, "cedar", t.TempDir(), "127.0.0.1:0")
	walnut := startDaemon(t, "walnut", t.TempDir(), "127.0.0.1:0")
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	join(t, cedar, walnut, cedar)

	var archive bytes.Buffer
	paths := backup.Paths{
		Database: filepath.Join(cedar.dataDir, DatabaseName),
		Key:      filepath.Join(cedar.dataDir, "node.key"),
		Cert:     filepath.Join(cedar.dataDir, "node.crt"),
	}
	if err := backup.Create(context.Background(), cedar.node.database, cedar.node.identity, paths, "a long backup passphrase", &archive); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// After the backup the owner publishes one more event, which walnut holds.
	cedar.must(http.MethodPost, "/admin/v1/members/"+walnut.node.NodeID()+"/promote", nil, nil)
	walnut.sync()
	address := cedar.address
	cedar.shutdown()

	restoredDir := t.TempDir()
	restored := backup.Paths{
		Database: filepath.Join(restoredDir, DatabaseName),
		Key:      filepath.Join(restoredDir, "node.key"),
		Cert:     filepath.Join(restoredDir, "node.crt"),
	}
	if _, err := backup.Restore(context.Background(), "a long backup passphrase", &archive, restored, "cedar"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	cedar = startDaemon(t, "cedar", restoredDir, address)
	if !cedar.status().Group.Held {
		t.Fatal("a restored node starts held")
	}
	// A held owner's own decision queues rather than being sequenced.
	var outcome Outcome
	cedar.must(http.MethodPost, "/admin/v1/members/"+walnut.node.NodeID()+"/demote", nil, &outcome)
	if outcome.Sequenced || !outcome.Queued {
		t.Fatalf("a held node must queue, not sequence: %+v", outcome)
	}

	if result := cedar.sync(); result.Applied == 0 {
		t.Fatal("the first heartbeat should fetch the event the backup lacked")
	}
	if !cedar.status().Group.Held {
		t.Fatal("a heartbeat that found newer events must not release the hold")
	}
	cedar.sync()
	if cedar.status().Group.Held {
		t.Fatal("a heartbeat with nothing newer from a majority should release the hold")
	}

	// The next heartbeat delivers the queued demotion, now built on the
	// complete log, and walnut accepts it: no equivocation.
	cedar.sync()
	if cedar.status().Group.Queued != 0 {
		t.Fatal("the queued decision should be sequenced once released")
	}
	walnut.sync()
	if cedar.status().Group.Sequence != walnut.status().Group.Sequence {
		t.Fatal("after release the two nodes should agree")
	}
	for _, member := range walnut.status().Group.Members {
		if member.NodeID == walnut.node.NodeID() && member.Administrator {
			t.Fatal("the demotion should have applied")
		}
	}
}

// C-PO-24: succession runs over the wire. With the owner gone for the full
// window, an ordinary member sends its signed attestation to the eligible
// successor, the successor claims once it holds a quorum, every member
// follows the new epoch, and the former owner returns as an ordinary member.
func TestSuccessionRunsOverTheWire(t *testing.T) {
	cedar := startDaemon(t, "cedar", t.TempDir(), "127.0.0.1:0")
	walnut := startDaemon(t, "walnut", t.TempDir(), "127.0.0.1:0")
	maple := startDaemon(t, "maple", t.TempDir(), "127.0.0.1:0")
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	join(t, cedar, walnut, cedar)
	join(t, cedar, maple, cedar)
	cedar.must(http.MethodPost, "/admin/v1/members/"+walnut.node.NodeID()+"/promote", nil, nil)
	walnut.sync()
	maple.sync()

	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := start
	for _, d := range []*testDaemon{walnut, maple} {
		d.node.SetClock(func() time.Time { return clock })
	}
	cedarDir, cedarAddress := cedar.dataDir, cedar.address
	cedar.shutdown()

	// The owner is first missed.
	walnut.sync()
	maple.sync()
	// Two weeks on, the window has not elapsed: nothing may happen.
	clock = start.Add(14 * 24 * time.Hour)
	maple.sync()
	walnut.sync()
	if walnut.status().Group.OwnerID != cedar.node.NodeID() {
		t.Fatal("succession must not happen before the window elapses")
	}

	clock = start.Add(15*24*time.Hour + time.Hour)
	walnut.sync() // eligible, but holds no attestation yet
	if walnut.status().Group.OwnerID != cedar.node.NodeID() {
		t.Fatal("the successor must not claim without a quorum of attestations")
	}
	maple.sync()  // attests to walnut
	walnut.sync() // claims with maple's attestation
	walnutStatus := walnut.status()
	if walnutStatus.Group.OwnerID != walnut.node.NodeID() || walnutStatus.Group.Epoch != 2 {
		t.Fatalf("walnut should own epoch 2: owner %q epoch %d", walnutStatus.Group.OwnerID, walnutStatus.Group.Epoch)
	}
	maple.sync()
	if maple.status().Group.OwnerID != walnut.node.NodeID() {
		t.Fatal("maple should follow the new owner")
	}

	// The former owner comes back and follows too.
	cedar = startDaemon(t, "cedar", cedarDir, cedarAddress)
	cedar.sync()
	cedarStatus := cedar.status()
	if cedarStatus.Group.OwnerID != walnut.node.NodeID() || cedarStatus.Group.Epoch != 2 {
		t.Fatalf("the returning owner should see the succession: owner %q epoch %d", cedarStatus.Group.OwnerID, cedarStatus.Group.Epoch)
	}
	if status := cedar.call(http.MethodPost, "/admin/v1/members/"+maple.node.NodeID()+"/promote", nil, &Outcome{}); status == http.StatusOK {
		t.Fatal("the former owner may no longer promote")
	}
}

func withServiceUser(fake *jellyfintest.Server, protected ...string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.JellyfinBaseURL, cfg.JellyfinUser, cfg.JellyfinPassword = fake.URL, "jellymesh", "service-user-password-for-tests"
		cfg.ProtectedLibraries = protected
	}
}

func (d *testDaemon) remote() map[string]RemoteLibrary {
	var libraries []RemoteLibrary
	d.must(http.MethodGet, "/admin/v1/remote", nil, &libraries)
	byLibrary := map[string]RemoteLibrary{}
	for _, library := range libraries {
		byLibrary[library.LibraryID] = library
	}
	return byLibrary
}

func (d *testDaemon) catalogSync() CatalogResult {
	var result CatalogResult
	d.must(http.MethodPost, "/admin/v1/catalog/sync", nil, &result)
	return result
}

// C-OP-7: catalogs flow between members through the daemon: publication with
// declared roots, a protected library refused, a join that offers the
// joiner's own publications, opting out and back in, a block, and ejection
// removing the ejected member's items. No service-user secret is audited.
func TestCatalogsFlowBetweenMembersThroughTheDaemon(t *testing.T) {
	cedarJellyfin, walnutJellyfin := jellyfintest.New(), jellyfintest.New()
	defer cedarJellyfin.Close()
	defer walnutJellyfin.Close()
	cedarJellyfin.AddLibrary("lib-movies", "Movies", "movies")
	cedarJellyfin.AddLibrary("lib-family", "Family Movies", "movies")
	cedarJellyfin.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-movies", "lib-family")
	for index := 0; index < 5; index++ {
		cedarJellyfin.AddItem("lib-movies", jellyfin.Item{ID: fmt.Sprintf("movie-%d", index), Name: fmt.Sprintf("Movie %d", index), Type: "Movie", Path: fmt.Sprintf("/media/movies/%d.mkv", index)})
	}
	cedarJellyfin.AddItem("lib-family", jellyfin.Item{ID: "family-1", Name: "Birthday", Type: "Movie", Path: "/media/family/1.mkv"})
	walnutJellyfin.AddLibrary("lib-docs", "Documentaries", "movies")
	walnutJellyfin.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-docs")
	walnutJellyfin.AddItem("lib-docs", jellyfin.Item{ID: "doc-1", Name: "Oceans", Type: "Movie", Path: "/media/docs/oceans.mkv"})

	cedar := startDaemonWith(t, "cedar", t.TempDir(), "127.0.0.1:0", withServiceUser(cedarJellyfin, "lib-family"))
	walnut := startDaemonWith(t, "walnut", t.TempDir(), "127.0.0.1:0", withServiceUser(walnutJellyfin))
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	cedar.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-movies", Roots: []string{"/media/movies"}}, nil)
	if status := cedar.call(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-family", Roots: []string{"/media/family"}}, nil); status == http.StatusOK {
		t.Fatal("a protected library must not be publishable")
	}

	// walnut has nothing to offer until it publishes.
	var invitation InviteResponse
	cedar.must(http.MethodPost, "/admin/v1/invitations", InviteRequest{ValidForSeconds: 3600}, &invitation)
	if status := walnut.call(http.MethodPost, "/admin/v1/join/redeem", RedeemAdminRequest{ShortCode: invitation.ShortCode, Address: invitation.Address}, nil); status == http.StatusOK {
		t.Fatal("a node that publishes nothing must not be able to redeem")
	}
	walnut.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-docs", Roots: []string{"/media/docs"}}, nil)
	joinOffering(t, cedar, walnut, cedar, nil)

	walnut.catalogSync()
	cedar.catalogSync()
	if movies := walnut.remote()["lib-movies"]; movies.ItemsHeld != 5 || movies.SourceID != cedar.node.NodeID() {
		t.Fatalf("walnut should hold cedar's 5 movies: %+v", movies)
	}
	if _, leaked := walnut.remote()["lib-family"]; leaked {
		t.Fatal("the protected library must never reach another member")
	}
	if docs := cedar.remote()["lib-docs"]; docs.ItemsHeld != 1 {
		t.Fatalf("cedar should hold walnut's documentary: %+v", docs)
	}

	walnut.must(http.MethodPut, "/admin/v1/optouts/"+cedar.node.NodeID()+"/lib-movies", nil, nil)
	cedarJellyfin.Update("movie-1", "Edited while walnut is opted out")
	cedar.catalogSync()
	if result := walnut.catalogSync(); result.Sources[cedar.node.NodeID()].Dropped != 0 || result.Sources[cedar.node.NodeID()].Applied != 0 {
		t.Fatalf("an opted-out library must not even be sent: %+v", result.Sources[cedar.node.NodeID()])
	}
	if movies := walnut.remote()["lib-movies"]; !movies.OptedOut || movies.ItemsHeld != 0 {
		t.Fatalf("after opting out: %+v", movies)
	}
	walnut.must(http.MethodDelete, "/admin/v1/optouts/"+cedar.node.NodeID()+"/lib-movies", nil, nil)
	walnut.catalogSync()
	if movies := walnut.remote()["lib-movies"]; movies.OptedOut || movies.ItemsHeld != 5 {
		t.Fatalf("after opting back in: %+v", movies)
	}

	cedar.must(http.MethodPut, "/admin/v1/blocks/"+walnut.node.NodeID(), nil, nil)
	if result := walnut.catalogSync(); result.Failed[cedar.node.NodeID()] == "" {
		t.Fatalf("a blocked member's catalog sync should fail: %+v", result)
	}
	if movies := walnut.remote()["lib-movies"]; movies.ItemsHeld != 5 {
		t.Fatal("a failed sync keeps what walnut already holds")
	}
	cedar.must(http.MethodDelete, "/admin/v1/blocks/"+walnut.node.NodeID(), nil, nil)

	// A block is symmetric: walnut blocking cedar stops walnut pulling from it.
	walnut.must(http.MethodPut, "/admin/v1/blocks/"+cedar.node.NodeID(), nil, nil)
	cedarJellyfin.Update("movie-2", "Edited while blocked")
	cedar.catalogSync()
	if result := walnut.catalogSync(); len(result.Sources) != 0 || len(result.Failed) != 0 {
		t.Fatalf("walnut must not contact a source it has blocked: %+v", result)
	}
	walnut.must(http.MethodDelete, "/admin/v1/blocks/"+cedar.node.NodeID(), nil, nil)

	cedar.must(http.MethodPost, "/admin/v1/members/"+walnut.node.NodeID()+"/eject", nil, nil)
	cedar.catalogSync()
	if _, held := cedar.remote()["lib-docs"]; held {
		t.Fatal("an ejected member's publications should be gone")
	}
	var remaining int
	cedar.node.database.SQL().QueryRow(`SELECT COUNT(*) FROM remote_items`).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("an ejected member's items should be removed, %d remain", remaining)
	}

	// C-SA-2: neither the service password nor its token was audited.
	events, _ := store.NewAuditRepository(cedar.node.database).List(context.Background(), 1000)
	secrets := cedar.node.jellyfin.Credentials()
	for _, event := range events {
		flat := event.Actor + event.Action + event.Subject
		for _, value := range event.Detail {
			flat += value
		}
		for _, secret := range secrets {
			if secret != "" && strings.Contains(flat, secret) {
				t.Fatalf("an audit event contains a service-user secret: %+v", event)
			}
		}
	}
	if len(events) == 0 {
		t.Fatal("expected audit events")
	}
}

// materializedStrm finds the one generated .strm whose name contains label.
func materializedStrm(t *testing.T, root string, label string) (string, string) {
	t.Helper()
	var found, content string
	filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".strm") && strings.Contains(filepath.Base(path), label) {
			data, _ := os.ReadFile(path)
			found, content = path, strings.TrimSpace(string(data))
		}
		return nil
	})
	return found, content
}

func relayGet(t *testing.T, relayBase string, strmURL string, rangeHeader string) (int, []byte) {
	t.Helper()
	target := relayBase + strmURL[strings.Index(strmURL, "/r/"):]
	request, _ := http.NewRequest(http.MethodGet, target, nil)
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, body
}

// C-OP-8: a remote item plays through the whole chain. cedar publishes it;
// walnut materializes a .strm, a subtitle, and a poster; a request to
// walnut's relay with the .strm's URL returns cedar's bytes, ranges included.
// Opting out, a block from either side, and a protected library all keep it
// from playing, and nothing protected is ever written.
func TestARemoteItemPlaysThroughTheWholeChain(t *testing.T) {
	cedarJellyfin, walnutJellyfin := jellyfintest.New(), jellyfintest.New()
	defer cedarJellyfin.Close()
	defer walnutJellyfin.Close()
	cedarJellyfin.AddLibrary("lib-movies", "Movies", "movies")
	cedarJellyfin.AddLibrary("lib-family", "Family Movies", "movies")
	cedarJellyfin.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-movies", "lib-family")
	cedarJellyfin.AddItem("lib-movies", jellyfin.Item{ID: "movie-1", Name: "Probe Film", ProductionYear: 2001, Type: "Movie",
		Path: "/media/movies/probe.mkv", ProviderIDs: map[string]string{"Tmdb": "603"}})
	media := bytes.Repeat([]byte("frame"), 300_000)
	cedarJellyfin.SetMedia("movie-1", media)
	cedarJellyfin.AddSubtitle("movie-1", 2, "eng", "1\n00:00:01,000 --> 00:00:02,000\nHello\n")
	cedarJellyfin.SetImage("movie-1", []byte("poster"))
	cedarJellyfin.AddItem("lib-family", jellyfin.Item{ID: "family-1", Name: "Birthday", Type: "Movie", Path: "/media/family/1.mkv"})
	cedarJellyfin.SetMedia("family-1", []byte("private"))
	walnutJellyfin.AddLibrary("lib-docs", "Documentaries", "movies")
	walnutJellyfin.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-docs")
	walnutJellyfin.AddItem("lib-docs", jellyfin.Item{ID: "doc-1", Name: "Oceans", Type: "Movie", Path: "/media/docs/oceans.mkv"})

	cedar := startDaemonWith(t, "cedar", t.TempDir(), "127.0.0.1:0", withServiceUser(cedarJellyfin, "lib-family"))
	walnutDir := t.TempDir()
	walnut := startDaemonWith(t, "walnut", walnutDir, "127.0.0.1:0", withServiceUser(walnutJellyfin))
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	cedar.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-movies", Roots: []string{"/media/movies"}}, nil)
	walnut.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-docs", Roots: []string{"/media/docs"}}, nil)
	joinOffering(t, cedar, walnut, cedar, nil)
	cedar.catalogSync()
	result := walnut.catalogSync()
	if result.Materialized.Written != 1 {
		t.Fatalf("walnut should materialize cedar's film: %+v", result.Materialized)
	}

	root := filepath.Join(walnutDir, "generated")
	strmPath, strmURL := materializedStrm(t, root, " - cedar")
	if strmPath == "" || !strings.HasPrefix(strmURL, "http://127.0.0.1:8090/r/") {
		t.Fatalf("no .strm for cedar's film, or wrong content: %q %q", strmPath, strmURL)
	}
	folder := filepath.Dir(strmPath)
	if _, err := os.Stat(filepath.Join(folder, strings.TrimSuffix(filepath.Base(strmPath), ".strm")+".eng.srt")); err != nil {
		t.Fatal("the subtitle should be copied beside the .strm")
	}
	if content, _ := os.ReadFile(filepath.Join(folder, "poster.jpg")); string(content) != "poster" {
		t.Fatal("the poster should be copied")
	}
	if walnutJellyfin.Notices() == 0 {
		t.Fatal("walnut should tell its Jellyfin what changed")
	}

	handler, err := walnut.node.RelayHandler()
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	relayServer := httptest.NewServer(handler)
	defer relayServer.Close()

	if status, body := relayGet(t, relayServer.URL, strmURL, ""); status != http.StatusOK || !bytes.Equal(body, media) {
		t.Fatalf("whole file through the relay: status %d, %d of %d bytes", status, len(body), len(media))
	}
	if status, body := relayGet(t, relayServer.URL, strmURL, "bytes=1000-1999"); status != http.StatusPartialContent || !bytes.Equal(body, media[1000:2000]) {
		t.Fatalf("range through the relay: status %d, %d bytes", status, len(body))
	}

	// Nothing of the protected library was written anywhere.
	filepath.WalkDir(root, func(path string, _ os.DirEntry, _ error) error {
		if strings.Contains(path, "Birthday") {
			t.Fatalf("a protected item was materialized: %s", path)
		}
		return nil
	})

	// Opting out withdraws it at once, before any catalog pass removes the
	// .strm: the relay asks the policy on every request.
	walnut.must(http.MethodPut, "/admin/v1/optouts/"+cedar.node.NodeID()+"/lib-movies", nil, nil)
	if status, _ := relayGet(t, relayServer.URL, strmURL, "bytes=0-9"); status != http.StatusNotFound {
		t.Fatalf("an opted-out item before the next pass: status %d, want 404", status)
	}
	walnut.catalogSync()
	if path, _ := materializedStrm(t, root, " - cedar"); path != "" {
		t.Fatal("an opted-out item's .strm must be removed")
	}
	if status, _ := relayGet(t, relayServer.URL, strmURL, ""); status != http.StatusNotFound {
		t.Fatalf("an opted-out item's old reference: status %d, want 404", status)
	}
	walnut.must(http.MethodDelete, "/admin/v1/optouts/"+cedar.node.NodeID()+"/lib-movies", nil, nil)
	walnut.catalogSync()
	_, strmURL = materializedStrm(t, root, " - cedar")
	if status, _ := relayGet(t, relayServer.URL, strmURL, "bytes=0-9"); status != http.StatusPartialContent {
		t.Fatalf("after opting back in: status %d", status)
	}

	// cedar blocks walnut: the source's listener answers not-found to a key it
	// no longer serves, and the relay passes the refusal on.
	cedar.must(http.MethodPut, "/admin/v1/blocks/"+walnut.node.NodeID(), nil, nil)
	if status, body := relayGet(t, relayServer.URL, strmURL, "bytes=0-9"); status != http.StatusNotFound || len(body) > 100 {
		t.Fatalf("a source that has blocked this node: status %d, want 404", status)
	}
	cedar.must(http.MethodDelete, "/admin/v1/blocks/"+walnut.node.NodeID(), nil, nil)

	// walnut blocks cedar: walnut's own policy refuses before any request,
	// and the next pass withdraws cedar's items, which a block alone does not
	// delete from the catalog.
	walnut.must(http.MethodPut, "/admin/v1/blocks/"+cedar.node.NodeID(), nil, nil)
	if status, _ := relayGet(t, relayServer.URL, strmURL, "bytes=0-9"); status != http.StatusNotFound {
		t.Fatalf("a source this node has blocked: status %d, want 404", status)
	}
	walnut.catalogSync()
	if path, _ := materializedStrm(t, root, " - cedar"); path != "" {
		t.Fatal("a blocked source's items must be withdrawn from the generated root")
	}
}
