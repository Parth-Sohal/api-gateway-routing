package main

import (
	"fmt"
	"net/http"
)

func hello(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Hello from API Gateway")
}

func users(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Users endpoint")
}

func main() {
	http.HandleFunc("/", hello)
	http.HandleFunc("/users", users)

	fmt.Println("Gateway running on port 8080")

	http.ListenAndServe(":8080", nil)
}