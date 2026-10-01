package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jellymesh/internal/config"
	"jellymesh/internal/mount"
)

// C-FS-9: status shows the configured presentation, never another, and in
// FUSE presentation the mount's latest report, with warnings for a mount
// that has not reported or whose propagation Jellyfin will not see.
func TestStatusShowsThePresentation(t *testing.T) {
	strm := startDaemonWith(t, "strm", t.TempDir(), "127.0.0.1:0", nil)
	var status Status
	strm.must(http.MethodGet, "/admin/v1/status", nil, &status)
	if status.Presentation.Mode != config.PresentStrm || status.Presentation.Mount != nil || status.Presentation.Reads != nil {
		t.Fatalf("strm presentation: %+v", status.Presentation)
	}

	fuse := startDaemonWith(t, "fuse", t.TempDir(), "127.0.0.1:0", func(cfg *config.Config) {
		cfg.Presentation, cfg.ReadCacheBytes = config.PresentFUSE, 64<<20
	})
	fuse.must(http.MethodGet, "/admin/v1/status", nil, &status)
	if status.Presentation.Mode != config.PresentFUSE || len(status.Presentation.Warnings) != 1 || !strings.Contains(status.Presentation.Warnings[0], "has not reported") {
		t.Fatalf("fuse presentation before the mount reports: %+v", status.Presentation)
	}

	reads := httptest.NewServer(fuse.node.ReadHandler())
	defer reads.Close()
	report := func(propagation string) {
		encoded, _ := json.Marshal(mount.Report{Mountpoint: "/presented", Propagation: propagation, RequestTimeout: true, Reads: 7})
		response, err := http.Post(reads.URL+mount.ReportPath, "application/json", bytes.NewReader(encoded))
		if err != nil || response.StatusCode != http.StatusNoContent {
			t.Fatalf("report: %v, %v", response, err)
		}
		response.Body.Close()
	}
	report("private")
	status = Status{}
	fuse.must(http.MethodGet, "/admin/v1/status", nil, &status)
	if status.Presentation.Mount == nil || status.Presentation.Mount.Reads != 7 || len(status.Presentation.Warnings) != 1 || !strings.Contains(status.Presentation.Warnings[0], "propagation is private") {
		t.Fatalf("a private mount should be warned about: %+v", status.Presentation)
	}
	report("shared")
	status = Status{}
	fuse.must(http.MethodGet, "/admin/v1/status", nil, &status)
	if len(status.Presentation.Warnings) != 0 || status.Presentation.MountSeen == "" || status.Presentation.Reads == nil {
		t.Fatalf("a healthy mount: %+v", status.Presentation)
	}
}
