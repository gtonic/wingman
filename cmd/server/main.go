package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"github.com/adrianliechti/wingman/config"
	"github.com/adrianliechti/wingman/pkg/connector"
	"github.com/adrianliechti/wingman/pkg/otel"
	"github.com/adrianliechti/wingman/server"
)

func main() {
	log.Println("DEBUG: cmd/server/main.go - main() started.") // Early debug log

	portFlag := flag.Int("port", 8080, "server port")
	addressFlag := flag.String("address", "", "server address")
	configFlag := flag.String("config", "config.yaml", "configuration path")

	flag.Parse()

	cfg, err := config.Parse(*configFlag)

	if err != nil {
		panic(err)
	}

	cfg.Address = fmt.Sprintf("%s:%d", *addressFlag, *portFlag)

	s, err := server.New(cfg)

	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Setup context that cancels on interrupt signals
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start configured connectors
	connectors := cfg.AllConnectors()
	if len(connectors) > 0 {
		log.Printf("Found %d connector(s) to start...", len(connectors))
		for id, conn := range connectors {
			go func(connectorID string, c connector.Provider) {
				log.Printf("Starting connector: %s", connectorID)
				if err := c.Start(ctx); err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						log.Printf("Connector %s failed: %v", connectorID, err)
					} else {
						log.Printf("Connector %s stopped: %v", connectorID, err)
					}
				} else {
					log.Printf("Connector %s completed without error.", connectorID)
				}
			}(id, conn)
		}
	} else {
		log.Println("No connectors configured to start.")
	}

	if err := otel.Setup("llama", "0.0.1"); err != nil {
		// For otel, perhaps log a warning instead of panic, or make it configurable
		log.Printf("Warning: Failed to setup OpenTelemetry: %v", err)
	}

	// Start the main server
	go func() {
		log.Printf("Server listening on %s", cfg.Address)
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server ListenAndServe failed: %v", err)
		}
	}()

	// Wait for interrupt signal
	<-ctx.Done()

	// Restore default behavior on the interrupt signal and notify user of shutdown.
	stop()
	log.Println("Shutting down server and connectors...")

	// Create a context with a timeout for graceful shutdown of the server
	// (Connectors are stopped by the cancellation of their context `ctx`)
	// The underlying server.Server does not have a Shutdown method, so we'll let it close when the app exits.
	// shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	// defer cancelShutdown()

	// if err := s.Shutdown(shutdownCtx); err != nil {
	// 	log.Printf("Server shutdown failed: %v", err)
	// }

	log.Println("Server exiting. Connectors have been signalled to stop.")
}
