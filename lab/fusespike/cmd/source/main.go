// Command source stands in for a remote source in the FUSE spike (#60): it
// serves files with byte ranges, counts every byte it sends, and can be told
// to misbehave (refuse, hang, crawl, or add latency) so the gates can check
// that none of that ever hangs or shrinks Jellyfin's library.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type faults struct {
	Mode      string `json:"mode"` // normal, refuse, hang, slow
	LatencyMS int    `json:"latency_ms"`
	RateKBps  int    `json:"rate_kbps"` // for slow; 0 = unlimited in other modes
}

var (
	root        string
	current     atomic.Value // faults
	bytesServed atomic.Int64
	requests    atomic.Int64
	perFile     sync.Map // name -> *atomic.Int64
)

func count(name string, n int64) {
	value, _ := perFile.LoadOrStore(name, new(atomic.Int64))
	value.(*atomic.Int64).Add(n)
	bytesServed.Add(n)
}

func serveFile(response http.ResponseWriter, request *http.Request) {
	f := current.Load().(faults)
	name := strings.TrimPrefix(request.URL.Path, "/files/")
	requests.Add(1)
	switch f.Mode {
	case "refuse":
		if hijacker, ok := response.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		http.Error(response, "refused", http.StatusServiceUnavailable)
		return
	case "hang":
		<-request.Context().Done()
		return
	}
	if f.LatencyMS > 0 {
		time.Sleep(time.Duration(f.LatencyMS) * time.Millisecond)
	}
	path := filepath.Join(root, filepath.Clean("/"+name))
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	defer file.Close()
	info, _ := file.Stat()
	size := info.Size()
	start, end := int64(0), size-1
	status := http.StatusOK
	if header := request.Header.Get("Range"); strings.HasPrefix(header, "bytes=") {
		parts := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)
		start, _ = strconv.ParseInt(parts[0], 10, 64)
		if parts[1] != "" {
			end, _ = strconv.ParseInt(parts[1], 10, 64)
		}
		if end >= size {
			end = size - 1
		}
		status = http.StatusPartialContent
		response.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	response.Header().Set("Accept-Ranges", "bytes")
	response.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	response.WriteHeader(status)
	if request.Method == http.MethodHead {
		return
	}
	file.Seek(start, 0)
	buffer := make([]byte, 64<<10)
	for left := end - start + 1; left > 0; {
		f = current.Load().(faults)
		step := int64(len(buffer))
		if f.Mode == "slow" && f.RateKBps > 0 {
			step = int64(f.RateKBps) * 1024 / 10
			if step < 1 {
				step = 1
			}
		}
		if step > left {
			step = left
		}
		n, err := file.Read(buffer[:step])
		if n > 0 {
			written, werr := response.Write(buffer[:n])
			count(name, int64(written))
			if werr != nil {
				return
			}
			left -= int64(n)
		}
		if err != nil {
			return
		}
		if f.Mode == "slow" {
			time.Sleep(100 * time.Millisecond)
		} else if f.Mode == "hang" {
			<-request.Context().Done()
			return
		}
	}
}

func main() {
	flag.StringVar(&root, "root", "/films", "directory served under /files/")
	listen := flag.String("listen", ":8300", "listen address")
	flag.Parse()
	current.Store(faults{Mode: "normal"})
	http.HandleFunc("/files/", serveFile)
	http.HandleFunc("/control", func(response http.ResponseWriter, request *http.Request) {
		var f faults
		if err := json.NewDecoder(request.Body).Decode(&f); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		if f.Mode == "" {
			f.Mode = "normal"
		}
		current.Store(f)
		log.Printf("faults now %+v", f)
	})
	http.HandleFunc("/stats", func(response http.ResponseWriter, request *http.Request) {
		files := map[string]int64{}
		perFile.Range(func(key, value any) bool {
			files[key.(string)] = value.(*atomic.Int64).Load()
			return true
		})
		out := map[string]any{"bytes": bytesServed.Load(), "requests": requests.Load(), "files": files}
		if request.URL.Query().Get("reset") == "1" {
			perFile.Range(func(key, _ any) bool { perFile.Delete(key); return true })
			bytesServed.Store(0)
			requests.Store(0)
		}
		json.NewEncoder(response).Encode(out)
	})
	log.Printf("source serving %s on %s", root, *listen)
	log.Fatal(http.ListenAndServe(*listen, nil))
}
