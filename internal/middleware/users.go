package middleware

import (
	"fmt"
	"net/http"
)

func UserMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		fmt.Println("User middleware called")

		next.ServeHTTP(w, r)
	})
}