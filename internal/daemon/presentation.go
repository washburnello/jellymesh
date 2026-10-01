package daemon

import (
	"encoding/json"
	"fmt"
	"time"

	"jellymesh/internal/config"
	"jellymesh/internal/mount"
)

// mountSilence is how long the mount may go without reporting before status
// warns that films cannot be read.
const mountSilence = 3 * mount.ReportInterval

// PresentationStatus is how remote items reach Jellyfin (A-17, A-18).
type PresentationStatus struct {
	// Mode is "strm" or "fuse", as configured. It never changes by itself.
	Mode string `json:"mode"`
	// Mount is the mount's latest report, in "fuse" mode.
	Mount      *mount.Report `json:"mount,omitempty"`
	MountSeen  string        `json:"mount_seen,omitempty"`
	Reads      *ReadStatus   `json:"reads,omitempty"`
	Warnings   []string      `json:"warnings,omitempty"`
	ReadSocket string        `json:"read_socket,omitempty"`
}

// ReadStatus counts the read service's work.
type ReadStatus struct {
	FetchedBytes int64 `json:"fetched_bytes"`
	ServedBytes  int64 `json:"served_bytes"`
	FetchErrors  int64 `json:"fetch_errors"`
	// FailedFilms are films whose read failed, retried until readable.
	FailedFilms int `json:"failed_films"`
}

func (n *Node) presentationStatus() PresentationStatus {
	status := PresentationStatus{Mode: n.cfg.Presentation}
	if status.Mode == "" {
		status.Mode = config.PresentStrm
	}
	if n.reads == nil {
		return status
	}
	status.ReadSocket = n.cfg.ReadSocket
	stats := n.reads.Stats()
	status.Reads = &ReadStatus{FetchedBytes: stats.Fetched, ServedBytes: stats.Served, FetchErrors: stats.FetchErrors, FailedFilms: stats.Failed}
	raw, seen := n.reads.MountReport()
	var report mount.Report
	if raw == nil || json.Unmarshal(raw, &report) != nil {
		status.Warnings = append(status.Warnings, "the mount has not reported since this node started; remote films cannot be read until it runs (jellymesh mount)")
		return status
	}
	status.Mount, status.MountSeen = &report, seen.UTC().Format(time.RFC3339)
	if silent := time.Since(seen); silent > mountSilence {
		status.Warnings = append(status.Warnings, fmt.Sprintf("the mount last reported %s ago; remote films cannot be read while it is down", silent.Round(time.Second)))
	}
	if report.Propagation != "shared" {
		status.Warnings = append(status.Warnings, fmt.Sprintf("the mount's propagation is %s, so Jellyfin may not see it after a remount; see docs/operator-fuse.md", report.Propagation))
	}
	if !report.RequestTimeout {
		status.Warnings = append(status.Warnings, mount.ErrNoRequestTimeout.Error())
	}
	return status
}
