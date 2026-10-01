// Package filmfile is the descriptor the materializer writes for each remote
// film when films are presented through the mount (design-spec section 11,
// A-17). The descriptor stands in the generated root where a .strm would,
// named after the film with Suffix added. The mount shows it without the
// suffix, as a read-only file of the film's size whose bytes come from the
// daemon by reference. It holds no address, key, or token.
package filmfile

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Suffix marks a descriptor: "Film - Cedar.mkv.jmfilm" is shown as
// "Film - Cedar.mkv".
const Suffix = ".jmfilm"

// MaxSize bounds a descriptor file, so the mount never reads a large file as
// one.
const MaxSize = 4096

// Descriptor is one film.
type Descriptor struct {
	// Reference is the item's relay reference, the same one a .strm names.
	Reference string `json:"reference"`
	// Size is the film's length in bytes, from the source's catalog.
	Size int64 `json:"size"`
	// Modified is the time the mount reports. It moves only when the film
	// changes, or forward an hour when a film whose read failed is readable
	// again, so that Jellyfin probes it again (A-17, G2-E).
	Modified int64 `json:"modified"`
	// Bitrate, in bits a second, is the film's average, if known.
	Bitrate int64 `json:"bitrate,omitempty"`
}

var referencePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

var ErrInvalid = errors.New("not a valid film descriptor")

// Encode returns the descriptor's file content.
func (descriptor Descriptor) Encode() []byte {
	encoded, _ := json.Marshal(descriptor)
	return append(encoded, '\n')
}

// Decode parses and checks a descriptor.
func Decode(content []byte) (Descriptor, error) {
	var descriptor Descriptor
	if len(content) > MaxSize {
		return Descriptor{}, ErrInvalid
	}
	if err := json.Unmarshal(content, &descriptor); err != nil {
		return Descriptor{}, ErrInvalid
	}
	if !referencePattern.MatchString(descriptor.Reference) || descriptor.Size <= 0 || descriptor.Bitrate < 0 {
		return Descriptor{}, ErrInvalid
	}
	return descriptor, nil
}

// ModifiedTime returns Modified as a time.
func (descriptor Descriptor) ModifiedTime() time.Time { return time.Unix(descriptor.Modified, 0) }

// Shown returns the name the mount shows for a descriptor's file name, and
// whether name is a descriptor at all.
func Shown(name string) (string, bool) {
	shown, found := strings.CutSuffix(name, Suffix)
	return shown, found && shown != "" && !strings.HasPrefix(shown, ".")
}

// Extensions are the file name extensions a film may be shown with: video
// containers Jellyfin recognizes. Anything else is shown as .mkv, since
// Jellyfin identifies the format by probing the content, not by the name.
var Extensions = map[string]bool{
	"mkv": true, "mp4": true, "m4v": true, "avi": true, "mov": true, "wmv": true, "ts": true,
	"m2ts": true, "mts": true, "webm": true, "mpg": true, "mpeg": true, "flv": true, "ogv": true,
	"3gp": true, "vob": true, "divx": true,
}

// Extension returns the extension a film is shown with.
func Extension(fromSource string) string {
	if Extensions[strings.ToLower(fromSource)] {
		return strings.ToLower(fromSource)
	}
	return "mkv"
}
