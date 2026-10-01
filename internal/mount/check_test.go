package mount

import (
	"strings"
	"testing"
)

const mountinfo = `22 1 0:21 / / rw,relatime - overlay overlay rw
23 22 0:22 / /proc rw,nosuid shared:5 - proc proc rw
30 22 8:1 /srv/presented /presented rw,relatime shared:12 - ext4 /dev/sda1 rw
31 22 8:1 /srv/generated /generated ro,relatime master:7 - ext4 /dev/sda1 rw
32 22 8:1 /srv/with\040space /with\040space rw,relatime master:3 shared:9 - ext4 /dev/sda1 rw
40 30 0:50 / /presented rw,nosuid shared:13 master:12 - fuse.jellymesh jellymesh rw
`

// C-FS-9: propagation is read from mountinfo for the mount holding a path,
// the deepest one, so the check and the mount's report say whether
// Jellyfin's container will see the mount.
func TestPropagationIsReadFromMountinfo(t *testing.T) {
	for path, want := range map[string]string{
		"/presented":           "shared",
		"/presented/Movies":    "shared",
		"/generated":           "slave",
		"/generated/Movies/x":  "slave",
		"/with space/a":        "shared",
		"/elsewhere":           "private",
		"/presentedness":       "private",
		"/proc/self/mountinfo": "shared",
	} {
		if got := propagation(strings.NewReader(mountinfo), path); got != want {
			t.Errorf("%s: %s, want %s", path, got, want)
		}
	}
	if got := propagation(strings.NewReader(""), "/x"); got != "unknown" {
		t.Errorf("no mountinfo: %s", got)
	}
}

// C-FS-9: the check reports each finding by name, and on this host finds the
// kernel's request timeouts.
func TestCheckReportsEachFinding(t *testing.T) {
	requireFUSE(t)
	findings, _ := Check(t.TempDir())
	byName := map[string]Finding{}
	for _, finding := range findings {
		byName[finding.Check] = finding
	}
	for _, name := range []string{"fuse device", "mount", "request timeouts", "propagation"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("no %q finding: %+v", name, findings)
		}
	}
	if !byName["request timeouts"].OK || !byName["mount"].OK {
		t.Fatalf("this host mounts and offers request timeouts: %+v", findings)
	}
}
