package main

import (
	"fmt"
	"log"
	"net/http"

	"api-gateway-routing/internal/middleware"
	"api-gateway-routing/internal/proxy"
)

func main() {

	loginProxy, err := proxy.NewProxy("http://localhost:9001")
	if err != nil {
		log.Fatal(err)
	}

	userProxy, err := proxy.NewProxy("http://localhost:9002")
	if err != nil {
		log.Fatal(err)
	}

	http.Handle("/login", loginProxy)

	http.Handle(
		"/users",
		middleware.UserMiddleware(userProxy), //specific midde ware for users
	)

	handler := middleware.Logging(http.DefaultServeMux) //common middleware working on all

	fmt.Println("Gateway running on :8080")

	log.Fatal(http.ListenAndServe(":8080", handler))
}