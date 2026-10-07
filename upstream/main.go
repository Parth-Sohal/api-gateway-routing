package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/", ordersHandler)

	fmt.Println("Orders service (upstream) starting on http://localhost:9000")
	err := http.ListenAndServe(":9000", nil)

	if err != nil {
		fmt.Println("Upstream failed:", err)
	}
}

func ordersHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Upstream received:", r.Method, r.URL.Path)
	fmt.Fprintln(w, "hello from orders service")
}
