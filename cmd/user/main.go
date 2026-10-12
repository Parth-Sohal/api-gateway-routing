package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	port := "9002"

	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Users handled by User Backend :%s\n", port)
	})

	fmt.Printf("User server running on :%s\n", port)

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal("Server error:", err)
	}
}