package main

import (
	"fmt"
	"net/http"
)

func users(w http.ResponseWriter, r *http.Request) {
    fmt.Fprintln(w, "Users handled by User Backend")
}

func main() {
    http.HandleFunc("/users", users)

    fmt.Println("User running on :9002")

    http.ListenAndServe(":9002", nil)
}