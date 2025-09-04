// Package index defines a minimal indexing interface and types used by the memory package.
// This local shim satisfies imports of "github.com/adrianliechti/wingman/pkg/index"
// via the replace directive in go.mod.
package index

import "context"

// Document represents a stored/retrieved memory item.
type Document struct {
	// Content is the textual payload.
	Content string
	// Metadata contains arbitrary key/value pairs associated with the document.
	Metadata map[string]string
	// Embedding optionally holds the vector representation of the content.
	Embedding []float32
}

// QueryOptions control retrieval behavior.
type QueryOptions struct {
	// Limit defines the maximum number of results to return.
	Limit *int
	// Filters constrains results by metadata.
	Filters map[string]string
}

// QueryResult represents a single retrieval hit.
type QueryResult struct {
	Document Document
	// Score is optional and can be used by implementations.
	Score float32
}

// Provider abstracts an index capable of storing and retrieving documents.
type Provider interface {
	// Index stores a document in the index.
	Index(ctx context.Context, doc Document) error
	// Query retrieves documents relevant to the query with optional options.
	Query(ctx context.Context, query string, opts *QueryOptions) ([]QueryResult, error)
}
