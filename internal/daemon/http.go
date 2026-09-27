package daemon

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"

	"jellymesh/internal/federation"
)

// federationRoutes serves the federation listener. The routes change when the
// node founds or joins its group, because that is when it gains an inviter,
// so the assembled handler is rebuilt then.
type federationRoutes struct {
	node    *Node
	mutex   sync.Mutex
	built   *groupRuntime
	handler http.Handler
}

func (routes *federationRoutes) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	runtime, _ := routes.node.current()
	routes.mutex.Lock()
	if routes.handler == nil || routes.built != runtime {
		services := []federation.Routes{routes.node.server}
		if runtime != nil {
			services = append(services, runtime.inviter, runtime.catalogServer)
		}
		routes.handler = federation.Handler(routes.node.server, services...)
		routes.built = runtime
	}
	handler := routes.handler
	routes.mutex.Unlock()
	handler.ServeHTTP(response, request)
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

func tlsListener(listener net.Listener, config *tls.Config) net.Listener {
	return tls.NewListener(listener, config)
}
