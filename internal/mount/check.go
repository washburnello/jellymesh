package mount

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Report is what the running mount tells the daemon, every ReportInterval,
// for `jellymesh status`.
type Report struct {
	Mountpoint     string    `json:"mountpoint"`
	Started        time.Time `json:"started"`
	AllowedUIDs    []uint32  `json:"allowed_uids"`
	RequestTimeout bool      `json:"request_timeout"`
	// Propagation is the mountpoint's propagation: "shared", "slave", or
	// "private". Jellyfin sees a remount only if it is shared.
	Propagation string `json:"propagation"`
	Reads       int64  `json:"reads"`
	ReadErrors  int64  `json:"read_errors"`
	Denied      int64  `json:"denied"`
}

// ReportInterval is how often the mount reports.
const ReportInterval = 30 * time.Second

// ReportPath is the read service's route for reports.
const ReportPath = "/v1/mount"

// Report returns the mount's current report.
func (m *Mount) Report() Report {
	stats := m.Stats()
	return Report{Mountpoint: m.options.Mountpoint, Started: m.started, AllowedUIDs: m.options.AllowedUIDs,
		RequestTimeout: m.requestTimeout, Propagation: Propagation(m.options.Mountpoint),
		Reads: stats.Reads, ReadErrors: stats.ReadErrors, Denied: stats.Denied}
}

// Propagation returns the propagation of the mount holding path, read from
// /proc/self/mountinfo: "shared", "slave", "private", or "unknown".
func Propagation(path string) string {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "unknown"
	}
	defer file.Close()
	return propagation(file, path)
}

func propagation(mountinfo io.Reader, path string) string {
	best, kind := "", "unknown"
	scanner := bufio.NewScanner(mountinfo)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		// id parent major:minor root mountpoint options [optional...] - type source super
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}
		point := unescape(fields[4])
		if !within(path, point) || len(point) < len(best) {
			continue
		}
		best, kind = point, "private"
		for _, optional := range fields[6:] {
			if optional == "-" {
				break
			}
			if strings.HasPrefix(optional, "shared:") {
				kind = "shared"
				break
			}
			if strings.HasPrefix(optional, "master:") {
				kind = "slave"
			}
		}
	}
	return kind
}

func within(path string, point string) bool {
	return point == "/" || path == point || strings.HasPrefix(path, point+"/")
}

// unescape undoes mountinfo's octal escapes of space, tab, newline, and
// backslash.
func unescape(field string) string {
	for _, pair := range [][2]string{{`\040`, " "}, {`\011`, "\t"}, {`\012`, "\n"}, {`\134`, `\`}} {
		field = strings.ReplaceAll(field, pair[0], pair[1])
	}
	return field
}

// Finding is one result of Check.
type Finding struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Check tests whether this host can present films through FUSE at
// mountpoint, as `jellymesh mount -check` runs it at setup and after each
// update. It mounts a throwaway filesystem in a temporary directory, so it
// needs the same privileges as the mount itself. It reports every finding,
// and OK only if all pass (A-18).
func Check(mountpoint string) ([]Finding, bool) {
	var findings []Finding
	add := func(check string, ok bool, detail string, args ...any) {
		findings = append(findings, Finding{Check: check, OK: ok, Detail: fmt.Sprintf(detail, args...)})
	}

	device, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		add("fuse device", false, "cannot open /dev/fuse: %v (pass the device to the container)", err)
	} else {
		device.Close()
		add("fuse device", true, "/dev/fuse opens")
	}

	directory, err := os.MkdirTemp("", "jellymesh-check-")
	if err != nil {
		add("mount", false, "cannot make a temporary directory: %v", err)
		return findings, false
	}
	defer os.Remove(directory)
	server, err := fs.Mount(directory, &fs.Inode{}, &fs.Options{MountOptions: fuse.MountOptions{
		DirectMount: true, FsName: "jellymesh-check", Name: "jellymesh", Options: []string{"ro"}, RequestTimeout: 20,
	}})
	if err != nil {
		add("mount", false, "cannot mount: %v (the mount container needs CAP_SYS_ADMIN, or run as root)", err)
	} else {
		add("mount", true, "a FUSE filesystem mounts")
		if server.KernelSettings().Flags64()&fuse.CAP_REQUEST_TIMEOUT != 0 {
			add("request timeouts", true, "the kernel offers FUSE request timeouts")
		} else {
			add("request timeouts", false, "%v", ErrNoRequestTimeout)
		}
		server.Unmount()
	}

	mountpoint = filepath.Clean(mountpoint)
	switch propagation := Propagation(mountpoint); propagation {
	case "shared":
		add("propagation", true, "%s is shared, so Jellyfin's container sees the mount and every remount", mountpoint)
	default:
		add("propagation", false, "%s is %s; bind it into this container with propagation rshared, and into Jellyfin's with rslave", mountpoint, propagation)
	}

	ok := true
	for _, finding := range findings {
		ok = ok && finding.OK
	}
	return findings, ok
}
