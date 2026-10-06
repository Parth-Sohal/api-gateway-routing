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

	fmt.Println("Gateway running on :8080")

	log.Fatal(http.ListenAndServe(":8080", nil))
}