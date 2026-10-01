// Command faultproxy stands between the gate lab's source node and its
// Jellyfin (#73). It passes everything through, counts the media bytes it
// relays, and can be told to make media requests misbehave (refuse, hang,
// crawl, or add latency), as the spike's fake source did (#60), so that the
// gates can be rerun against the real chain.
//
//	GET  /stats                 bytes and requests, per item; ?reset=1 zeroes them
//	POST /control {"mode":"normal|refuse|hang|slow","latency_ms":N,"rate_kbps":N}
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type faults struct {
	Mode      string `json:"mode"`
	LatencyMS int    `json:"latency_ms"`
	RateKBps  int    `json:"rate_kbps"`
}

var (
	current  atomic.Value // faults
	bytes    atomic.Int64
	requests atomic.Int64
	perItem  sync.Map // item -> *atomic.Int64
)

// media reports whether a request is for an item's media, and which item.
func media(path string) (string, bool) {
	// /Videos/{item}/stream, the route Jellymesh's source reads.
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 3 && strings.EqualFold(parts[0], "Videos") && strings.HasPrefix(strings.ToLower(parts[2]), "stream") {
		return parts[1], true
	}
	return "", false
}

type countingBody struct {
	io.ReadCloser
	item string
	rate int // bytes a second, 0 for no limit
}

func (body *countingBody) Read(p []byte) (int, error) {
	if body.rate > 0 && len(p) > body.rate/10+1 {
		p = p[:body.rate/10+1]
	}
	n, err := body.ReadCloser.Read(p)
	if n > 0 {
		bytes.Add(int64(n))
		value, _ := perItem.LoadOrStore(body.item, new(atomic.Int64))
		value.(*atomic.Int64).Add(int64(n))
		if body.rate > 0 {
			time.Sleep(time.Duration(n) * time.Second / time.Duration(body.rate))
		}
	}
	return n, err
}

func main() {
	listen := flag.String("listen", ":8096", "where the source node connects")
	control := flag.String("control", ":8300", "fault control and stats")
	target := flag.String("target", "http://fl-src-jf:8096", "the source Jellyfin")
	flag.Parse()
	current.Store(faults{Mode: "normal"})
	upstream, err := url.Parse(*target)
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(response *http.Response) error {
		if item, ok := media(response.Request.URL.Path); ok {
			f := current.Load().(faults)
			rate := 0
			if f.Mode == "slow" {
				rate = f.RateKBps * 1000
			}
			response.Body = &countingBody{ReadCloser: response.Body, item: item, rate: rate}
		}
		return nil
	}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if _, ok := media(request.URL.Path); ok {
			requests.Add(1)
			f := current.Load().(faults)
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
		}
		proxy.ServeHTTP(response, request)
	})
	controls := http.NewServeMux()
	controls.HandleFunc("/control", func(response http.ResponseWriter, request *http.Request) {
		var f faults
		if err := json.NewDecoder(request.Body).Decode(&f); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		current.Store(f)
		log.Printf("faults: %+v", f)
	})
	controls.HandleFunc("/stats", func(response http.ResponseWriter, request *http.Request) {
		items := map[string]int64{}
		perItem.Range(func(key, value any) bool { items[key.(string)] = value.(*atomic.Int64).Load(); return true })
		json.NewEncoder(response).Encode(map[string]any{"bytes": bytes.Load(), "requests": requests.Load(), "items": items, "faults": current.Load()})
		if request.URL.Query().Get("reset") == "1" {
			perItem.Range(func(key, _ any) bool { perItem.Delete(key); return true })
			bytes.Store(0)
			requests.Store(0)
		}
	})
	go func() { log.Fatal(http.ListenAndServe(*control, controls)) }()
	log.Printf("faultproxy %s -> %s, control on %s", *listen, *target, *control)
	log.Fatal(http.ListenAndServe(*listen, handler))
}
