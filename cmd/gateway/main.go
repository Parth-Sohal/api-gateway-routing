package main

import (
	"fmt"
	"log"
	"net/http"

	"api-gateway-routing/internal/middleware"
	"api-gateway-routing/internal/proxy"
	"api-gateway-routing/internal/ratelimiter"
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

	limiter := ratelimiter.NewRateLimiter(5, 1)

	http.Handle("/login", loginProxy)

	http.Handle(
		"/users",
		middleware.UserMiddleware(userProxy),
	)

	handler := middleware.Logging(
		middleware.RateLimit(limiter, http.DefaultServeMux),
	)

	fmt.Println("Gateway running on :8080")

	log.Fatal(http.ListenAndServe(":8080", handler))
}