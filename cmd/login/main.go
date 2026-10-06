package main

import (
	"fmt"
	"log"
	"net/http"

	"api-gateway-routing/internal/proxy"
)

func main() {

	loginProxy, err := proxy.NewProxy("http://localhost:9000")
	if err != nil {
		log.Fatal(err)
	}

	http.Handle("/login", loginProxy)

	fmt.Println("Login running on :9001")

	log.Fatal(http.ListenAndServe(":9001", nil))
}