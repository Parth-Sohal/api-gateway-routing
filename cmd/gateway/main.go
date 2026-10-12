package main

import (
	"fmt"
	"log"
	"net/http"

	"api-gateway-routing/internal/loadbalancer"
	"api-gateway-routing/internal/middleware"
	"api-gateway-routing/internal/proxy"
	"api-gateway-routing/internal/ratelimiter"
)

func main() {

	// Login Service
	loginProxy, err := proxy.NewProxy("http://localhost:9001")
	if err != nil {
		log.Fatal(err)
	}

	// User Service instances
	userServers := []string{
		"http://localhost:9002",
		"http://localhost:9003",
		"http://localhost:9004",
	}

	// Create Round-Robin Load Balancer
	userLB, err := loadbalancer.NewLoadBalancer(userServers)
	if err != nil {
		log.Fatal(err)
	}

	// Rate Limiter
	limiter := ratelimiter.NewRateLimiter(5, 1)

	// Routes
	http.Handle("/login", loginProxy)

	http.Handle(
		"/users",
		middleware.UserMiddleware(userLB),
	)

	// Middleware chain
	handler := middleware.Logging(
		middleware.RateLimit(limiter, http.DefaultServeMux),
	)

	fmt.Println("Gateway running on :8080")

	log.Fatal(http.ListenAndServe(":8080", handler))
}