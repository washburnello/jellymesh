// Command jellymesh runs a Jellymesh node and administers it.
//
//	jellymesh serve                         run the node
//	jellymesh status                        show the node and its group
//	jellymesh found <group-id>              create a group owned by this node
//	jellymesh invite [-valid-for 24h]       issue an invitation
//	jellymesh join -address host:port -code CODE -library id:name:type ...
//	jellymesh join -qr 'jellymesh:1:...' -library id:name:type ...
//	jellymesh requests                      list pending join requests
//	jellymesh approve <inviter-id> <invitation-id>
//	jellymesh deny <inviter-id> <invitation-id>
//	jellymesh eject|promote|demote <node-id>
//	jellymesh leave
//	jellymesh block|unblock <node-id>
//	jellymesh sync                          run one heartbeat now
//	jellymesh backup <file>                 write an encrypted backup
//	jellymesh restore <file>                restore a backup into an empty data directory
//	jellymesh healthcheck                   exit 0 if the local node answers (for container health checks)
//
// Every command but serve, backup, and restore talks to the running node over
// its loopback admin API, authenticated by the token in the data directory.
// backup and restore read JELLYMESH_BACKUP_PASSPHRASE.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"jellymesh/internal/backup"
	"jellymesh/internal/config"
	"jellymesh/internal/daemon"
	"jellymesh/internal/node"
	"jellymesh/internal/policy"
	"jellymesh/internal/store"
	"jellymesh/internal/syncpolicy"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	command, args := os.Args[1], os.Args[2:]
	if err := run(cfg, command, args); err != nil {
		fmt.Fprintln(os.Stderr, "jellymesh:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: jellymesh serve|status|found|invite|join|requests|approve|deny|eject|promote|demote|leave|block|unblock|sync|backup|restore|healthcheck")
	os.Exit(2)
}

func run(cfg config.Config, command string, args []string) error {
	switch command {
	case "serve":
		return serve(cfg)
	case "backup":
		return writeBackup(cfg, args)
	case "restore":
		return restore(cfg, args)
	case "healthcheck":
		return healthcheck(cfg)
	}

	admin, err := newAdminClient(cfg)
	if err != nil {
		return err
	}
	switch command {
	case "status":
		return admin.print(http.MethodGet, "/admin/v1/status", nil)
	case "found":
		if len(args) != 1 {
			return errors.New("usage: jellymesh found <group-id>")
		}
		return admin.print(http.MethodPost, "/admin/v1/group", daemon.FoundRequest{GroupID: args[0]})
	case "invite":
		flags := flag.NewFlagSet("invite", flag.ExitOnError)
		validFor := flags.Duration("valid-for", 24*time.Hour, "how long the invitation can be redeemed")
		flags.Parse(args)
		return admin.print(http.MethodPost, "/admin/v1/invitations", daemon.InviteRequest{ValidForSeconds: int(validFor.Seconds())})
	case "join":
		return join(admin, args)
	case "requests":
		return admin.print(http.MethodGet, "/admin/v1/requests", nil)
	case "approve", "deny":
		if len(args) != 2 {
			return fmt.Errorf("usage: jellymesh %s <inviter-id> <invitation-id>", command)
		}
		return admin.print(http.MethodPost, "/admin/v1/requests/"+command, daemon.DecisionRequest{InviterID: args[0], InvitationID: args[1]})
	case "eject", "promote", "demote":
		if len(args) != 1 {
			return fmt.Errorf("usage: jellymesh %s <node-id>", command)
		}
		return admin.print(http.MethodPost, "/admin/v1/members/"+args[0]+"/"+command, nil)
	case "leave":
		return admin.print(http.MethodPost, "/admin/v1/leave", nil)
	case "block", "unblock":
		if len(args) != 1 {
			return fmt.Errorf("usage: jellymesh %s <node-id>", command)
		}
		method := http.MethodPut
		if command == "unblock" {
			method = http.MethodDelete
		}
		return admin.print(method, "/admin/v1/blocks/"+args[0], nil)
	case "sync":
		return admin.print(http.MethodPost, "/admin/v1/sync", nil)
	}
	usage()
	return nil
}

func serve(cfg config.Config) error {
	if err := os.MkdirAll(cfg.DataDirectory, 0o700); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	n, err := daemon.Open(ctx, cfg, log.Default())
	if err != nil {
		return err
	}
	defer n.Close()
	token, err := daemon.LoadOrCreateAdminToken(filepath.Join(cfg.DataDirectory, daemon.AdminTokenName))
	if err != nil {
		return err
	}

	adminListener, err := net.Listen("tcp", cfg.AdminListenAddress)
	if err != nil {
		return err
	}
	adminServer := &http.Server{Handler: n.AdminHandler(token), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := adminServer.Serve(adminListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("admin: %v", err)
		}
	}()
	defer adminServer.Close()

	federationListener, err := net.Listen("tcp", cfg.FederationListenAddress)
	if err != nil {
		return err
	}
	log.Printf("jellymesh node %s (%s) federating on %s as %s, admin on %s",
		n.NodeID(), n.Identity().Fingerprint(), cfg.FederationListenAddress, cfg.PublicAddress(), cfg.AdminListenAddress)
	return n.Run(ctx, federationListener, syncpolicy.DefaultHealthInterval)
}

type adminClient struct {
	base  string
	token string
}

func newAdminClient(cfg config.Config) (*adminClient, error) {
	data, err := os.ReadFile(filepath.Join(cfg.DataDirectory, daemon.AdminTokenName))
	if err != nil {
		return nil, fmt.Errorf("read the admin token (is the node running, and is this its data directory?): %w", err)
	}
	return &adminClient{base: "http://" + cfg.AdminListenAddress, token: strings.TrimSpace(string(data))}, nil
}

func (admin *adminClient) call(method string, path string, body any, into any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, admin.base+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+admin.token)
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	if into != nil {
		return json.Unmarshal(data, into)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil {
		data = pretty.Bytes()
	}
	fmt.Println(strings.TrimSpace(string(data)))
	return nil
}

func (admin *adminClient) print(method string, path string, body any) error {
	return admin.call(method, path, body, nil)
}

type libraryFlags []policy.Library

func (libraries *libraryFlags) String() string { return fmt.Sprint(*libraries) }

func (libraries *libraryFlags) Set(value string) error {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 {
		return errors.New("a library is id:name:collection-type, for example movies:Movies:movies")
	}
	*libraries = append(*libraries, policy.Library{ID: parts[0], Name: parts[1], CollectionType: parts[2]})
	return nil
}

// join redeems an invitation, then waits for the decision and joins.
func join(admin *adminClient, args []string) error {
	flags := flag.NewFlagSet("join", flag.ExitOnError)
	address := flags.String("address", "", "the inviter's address, with a short code")
	code := flags.String("code", "", "the invitation's short code")
	qr := flags.String("qr", "", "the invitation's QR text")
	wait := flags.Duration("wait", 24*time.Hour, "how long to wait for an owner or administrator to decide")
	var libraries libraryFlags
	flags.Var(&libraries, "library", "a library to publish, as id:name:collection-type (repeatable; at least one)")
	flags.Parse(args)

	var pending daemon.PendingJoin
	if err := admin.call(http.MethodPost, "/admin/v1/join/redeem", daemon.RedeemAdminRequest{
		ShortCode: *code, Address: *address, QR: *qr, Libraries: libraries,
	}, &pending); err != nil {
		return err
	}
	fmt.Printf("Redeemed. The inviter's key is %s.\nWaiting for an owner or administrator to approve invitation %s...\n", pending.InviterFingerprint, pending.InvitationID)
	deadline := time.Now().Add(*wait)
	for {
		var result daemon.PendingJoin
		if err := admin.call(http.MethodPost, "/admin/v1/join/complete", pending, &result); err != nil {
			return err
		}
		switch result.Status {
		case string(policy.InvitationAdmitted):
			fmt.Printf("Admitted to %s.\n", result.GroupID)
			return nil
		case string(policy.InvitationDenied):
			return errors.New("the request was denied")
		}
		if time.Now().After(deadline) {
			return errors.New("no decision yet; run the same join again later to keep waiting")
		}
		time.Sleep(5 * time.Second)
	}
}

func backupPaths(cfg config.Config) backup.Paths {
	return backup.Paths{
		Database: filepath.Join(cfg.DataDirectory, daemon.DatabaseName),
		Key:      cfg.NodeKeyPath,
		Cert:     cfg.NodeCertPath,
	}
}

func passphrase() (string, error) {
	value := os.Getenv("JELLYMESH_BACKUP_PASSPHRASE")
	if value == "" {
		return "", errors.New("set JELLYMESH_BACKUP_PASSPHRASE")
	}
	return value, nil
}

func writeBackup(cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: jellymesh backup <file>")
	}
	secret, err := passphrase()
	if err != nil {
		return err
	}
	paths := backupPaths(cfg)
	identity, err := node.LoadOrCreate(paths.Key, paths.Cert, cfg.NodeName)
	if err != nil {
		return err
	}
	database, err := store.Open(paths.Database)
	if err != nil {
		return err
	}
	defer database.Close()
	file, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := backup.Create(context.Background(), database, identity, paths, secret, file); err != nil {
		file.Close()
		os.Remove(args[0])
		return err
	}
	return file.Close()
}

func restore(cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: jellymesh restore <file>")
	}
	secret, err := passphrase()
	if err != nil {
		return err
	}
	file, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer file.Close()
	manifest, err := backup.Restore(context.Background(), secret, file, backupPaths(cfg), cfg.NodeName)
	if err != nil {
		return err
	}
	fmt.Printf("Restored node %s from a backup made %s.\nIt will not sequence until it has caught up with its peers.\n",
		manifest.Fingerprint, manifest.CreatedAt.Format(time.RFC3339))
	return nil
}

// healthcheck asks the local admin listener whether the node is up. It needs
// no token: /healthz reports only that the process answers.
func healthcheck(cfg config.Config) error {
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + cfg.AdminListenAddress + "/healthz")
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health check answered %s", response.Status)
	}
	return nil
}
