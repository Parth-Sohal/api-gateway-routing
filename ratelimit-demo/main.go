package main

import (
	"fmt"
	"time"
)

func main() {
	// 3 tokens capacity, refill 1 token per second
	rl := NewRateLimiter(3, 1)

	fmt.Println("--- burst test (expect 3 allowed, then denials) ---")
	for i := 1; i <= 5; i++ {
		allowed := rl.Allow("client-A")
		fmt.Printf("request %d: allowed=%v\n", i, allowed)
	}

	fmt.Println("--- waiting 2 seconds for refill ---")
	time.Sleep(2 * time.Second)

	fmt.Println("--- after refill (expect 2 allowed, then denial) ---")
	for i := 1; i <= 3; i++ {
		allowed := rl.Allow("client-A")
		fmt.Printf("request %d: allowed=%v\n", i, allowed)
	}

	fmt.Println("--- different client (expect allowed) ---")
	fmt.Printf("client-B first request: allowed=%v\n", rl.Allow("client-B"))
}
