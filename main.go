package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func main() {
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/hello", helloHandler)
	http.HandleFunc("/version", versionHandler)
	http.HandleFunc("/orders", ordersProxyHandler) // <-- NEW

	fmt.Println("Gateway starting on http://localhost:8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		fmt.Println("Gateway failed:", err)
	}
}

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

// -------- NEW: the proxy handler --------
// gate way - > client + server

func ordersProxyHandler(w http.ResponseWriter, r *http.Request) {

	// STEP A: gateway acts as a CLIENT to the upstream
	resp, err := http.Get("http://localhost:9000/") // it becomes a client

	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}

	defer resp.Body.Close()
	// defer -> close when the func finsihes
	// close the stream of data

	// STEP B: gateway acts as a SERVER to the client
	//         (copies the upstream's body into our response)
	io.Copy(w, resp.Body)
	// Take everything from the backend's response body and copy it into the response we're
	// sending to the client.
}

func usersProxyHandler(w http.ResponseWriter, r *http.Request) {
	resp, err := http.Get("http://localhost:9001/")
	if err != nil {
		http.Error(w, "users upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	io.Copy(w, resp.Body)
}
