package main

import (
	"fmt"
	"net/http"
)

func main() {

	http.HandleFunc("/", userHandler)

	fmt.Println("User service (upstream) starting on http://localhost:9001")

	err := http.ListenAndServe(":9001", nil)

	if err != nil {
		fmt.Println("Upstream-User failed:", err)
	}

}

func userHandler(w http.ResponseWriter, r *http.Request) {

	fmt.Println("Upstream received:", r.Method, r.URL.Path)
	fmt.Fprintln(w, "hello from Users service")
}
