package loadbalancer

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

type LoadBalancer struct {
	servers []*httputil.ReverseProxy
	current int
	mu      sync.Mutex
}

func NewLoadBalancer(serverURLs []string) (*LoadBalancer, error) {
	lb := &LoadBalancer{
		servers: make([]*httputil.ReverseProxy, 0, len(serverURLs)),
	}

	for _, serverURL := range serverURLs {
		target, err := url.Parse(serverURL)
		if err != nil {
			return nil, err
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		lb.servers = append(lb.servers, proxy)
	}

	return lb, nil
}

func (lb *LoadBalancer) Next() *httputil.ReverseProxy {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	server := lb.servers[lb.current]
	lb.current = (lb.current + 1) % len(lb.servers)

	return server
}

func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lb.Next().ServeHTTP(w, r)
}