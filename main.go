package main

import (
	"fmt"
	"gofer.email/internal/handler"
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()
	h := handler.New()
	h.RegisterRoutes(mux)

	addr := ":8090"
	fmt.Printf("gofer.email running on http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
