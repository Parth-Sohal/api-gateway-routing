package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// HTTP client used by our gateway to communicate with backend services.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

// Route represents one gateway route.
type Route struct {
	Path     string `json:"path"`
	Upstream string `json:"upstream"`
}

func main() {

	// Load routes from config.json
	routes, err := loadRoutes("config.json")
	if err != nil {
		fmt.Println("Failed to load config:", err)
		return
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	defer rdb.Close()

	rl := NewRateLimiter(rdb, 10, 2) // capacity 10, refill 2 tokens/sec

	// Register every proxy route.
	for _, route := range routes {

		// Create a handler that remembers this route's upstream.
		handler := makeProxyHandler(route.Upstream, rl)

		// Register the handler with Go's HTTP router.
		http.HandleFunc(route.Path, handler)

		fmt.Printf(
			"Route registered: %s -> %s\n",
			route.Path,
			route.Upstream,
		)
	}

	// Normal gateway routes.
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/hello", helloHandler)
	http.HandleFunc("/version", versionHandler)

	fmt.Println("Gateway starting on http://localhost:8081")

	err = http.ListenAndServe(":8081", nil)

	if err != nil {
		fmt.Println("Gateway failed:", err)
	}
}

// ---------------- CONFIG LOADING ----------------

func loadRoutes(path string) ([]Route, error) {

	// Read the configuration file.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Structure matching the JSON configuration.
	var cfg struct {
		Routes []Route `json:"routes"`
	}

	// Convert JSON into Go structs.
	err = json.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}

	return cfg.Routes, nil
}

// ---------------- PROXY ----------------

func makeProxyHandler(upstream string, rl *RateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		// --- Rate limiting ---
		clientIP := strings.Split(r.RemoteAddr, ":")[0]

		if !rl.Allow(clientIP) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		// --- Proxy ---
		outReq, err := http.NewRequest(r.Method, upstream+r.URL.RequestURI(), r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		for name, values := range r.Header {
			for _, value := range values {
				outReq.Header.Add(name, value)
			}
		}

		resp, err := httpClient.Do(outReq)
		if err != nil {
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for name, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}

		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}

// ---------------- LOCAL HANDLERS ----------------

func healthHandler(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Content-Type", "application/json")

	json.NewEncoder(w).Encode(
		map[string]string{
			"status": "ok",
		},
	)
}

func helloHandler(w http.ResponseWriter, r *http.Request) {

	name := r.URL.Query().Get("name")

	if name == "" {
		name = "stranger"
	}

	fmt.Fprintf(w, "hello %s\n", name)
}

func versionHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "version V1.0")
}
