package connector

import "context"

// Provider defines the interface for a connector that can be started and stopped.
// Connectors are typically long-running services that integrate with external systems.
type Provider interface {
	// Start initiates the connector's operation.
	// It should run until the context is cancelled or an unrecoverable error occurs.
	Start(ctx context.Context) error

	// ID returns the unique identifier of this connector instance.
	// This is typically the key used in the configuration.
	ID() string
}
