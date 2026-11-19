// Command idempotency-example shows the idempotency middleware protecting a
// payments endpoint. Run it, then retry the same POST with the same
// Idempotency-Key: the second response is the stored one, marked with
// Idempotency-Replayed: true, and the handler does not run again.
//
//	go run .            # from this directory
//
//	# first request runs the handler:
//	curl -i -X POST localhost:8080/payments \
//	  -H 'Idempotency-Key: abc-123' -d '{"amount":100}'
//
//	# retry replays the stored response:
//	curl -i -X POST localhost:8080/payments \
//	  -H 'Idempotency-Key: abc-123' -d '{"amount":100}'
package main

import (
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/its-ernest/http-keystone/idempotency"
	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
)

func main() {
	e := echo.New()

	// A single in-memory store backs the idempotency records. In production,
	// swap in store.NewRedisStore so every instance shares the same records.
	memStore := store.NewMemoryStore()

	// Make unsafe requests safe to retry. The first request with a given
	// Idempotency-Key runs the handler; retries with the same key replay the
	// stored response instead of running the handler again.
	e.Use(idempotency.New(idempotency.Config{
		Store: memStore,
		TTL:   24 * time.Hour,
	}))

	// A counter so the example makes the "runs once" behaviour visible.
	var charges atomic.Int64

	e.POST("/payments", func(c *echo.Context) error {
		n := charges.Add(1)
		log.Printf("[handler] charging (call #%d)", n)
		return c.JSON(http.StatusCreated, map[string]any{
			"charge":  n,
			"message": "payment created",
		})
	})

	log.Println("idempotency example listening on :8080")
	if err := e.Start(":8080"); err != nil {
		log.Fatalf("server failed: %s", err)
	}
}
