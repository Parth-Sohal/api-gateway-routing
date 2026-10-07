package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// --- Types ---
type Route struct {
	Path     string `json:"path"`
	Upstream string `json:"upstream"`
}

// --- Entry point ---
func main() {
	routes, err := loadRoutes("config.json")
	if err != nil {
		fmt.Println("Failed to load config:", err)
		return
	}

	for _, route := range routes {
		handler := makeProxyHandler(route.Upstream)
		http.HandleFunc(route.Path, handler)
		fmt.Printf("Route registered: %s -> %s\n", route.Path, route.Upstream)
	}

	// normal routes
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/hello", helloHandler)
	http.HandleFunc("/version", versionHandler)

	fmt.Println("Gateway starting on http://localhost:8080")
	err = http.ListenAndServe(":8080", nil)
	if err != nil {
		fmt.Println("Gateway failed:", err)
	}
}

// --- Config loading ---
func loadRoutes(path string) ([]Route, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg struct {
		Routes []Route `json:"routes"`
	}

	err = json.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}

	return cfg.Routes, nil
}

// --- Proxy factory ---
func makeProxyHandler(upstream string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		outReq, err := http.NewRequest(r.Method, upstream+r.URL.Path, r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		resp, err := http.DefaultClient.Do(outReq)
		if err != nil {
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		io.Copy(w, resp.Body)
	}
}

// --- Local handlers (unchanged) ---
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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
