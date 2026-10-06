package main

import (
	"fmt"
	"net/http"
)

func login(w http.ResponseWriter, r *http.Request) {
    fmt.Fprintln(w, "login handled by User Backend")
}

func main() {
    http.HandleFunc("/login", login)

    fmt.Println("Login running on :9001")

    http.ListenAndServe(":9001", nil)
}