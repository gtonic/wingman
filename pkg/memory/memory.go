// Package memory: memory integration for logging and recall in chat completions.
package memory

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/adrianliechti/wingman/pkg/index"
)

type MemoryProvider = index.Provider

type MemoryConfig struct {
	Index            string
	RecallK          int
	LogConversations bool
	InjectMemories   bool
}

type MemoryManager struct {
	memory *MemoryConfig
	index  index.Provider
}

// Config returns the memory configuration.
func (m *MemoryManager) Config() *MemoryConfig {
	return m.memory
}

// NewMemoryManager creates a new MemoryManager with the given config and index provider.
func NewMemoryManager(mem *MemoryConfig, idx index.Provider) *MemoryManager {
	if mem == nil || idx == nil {
		return &MemoryManager{memory: mem, index: nil}
	}
	return &MemoryManager{memory: mem, index: idx}
}

// LogTurn logs a conversation turn to memory if enabled.
func (m *MemoryManager) LogTurn(ctx context.Context, user, assistant string, meta map[string]string, embedding []float32) error {
	if m.memory == nil || !m.memory.LogConversations || m.index == nil {
		return nil
	}
	log.Printf("[Memory] Storing conversation turn. User: %q, Assistant: %q, Metadata: %+v", user, assistant, meta)
	content := fmt.Sprintf("User: %s\nAssistant: %s", user, assistant)
	doc := index.Document{
		Content:   content,
		Metadata:  meta,
		Embedding: embedding,
	}
	return m.index.Index(ctx, doc)
}

// RecallMemories queries memory for relevant context.
func (m *MemoryManager) RecallMemories(ctx context.Context, query string, meta map[string]string) ([]index.Document, error) {
	if m.memory == nil || !m.memory.InjectMemories || m.index == nil {
		return nil, nil
	}
	log.Printf("[Memory] Recalling memories. Query: %q, Metadata: %+v", query, meta)
	opts := &index.QueryOptions{
		Limit:   &m.memory.RecallK,
		Filters: meta,
	}
	results, err := m.index.Query(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	log.Printf("[Memory] Recall returned %d results.", len(results))
	docs := make([]index.Document, 0, len(results))
	for _, r := range results {
		docs = append(docs, r.Document)
	}
	return docs, nil
}

// InjectMemories prepends recalled memories to the prompt.
func InjectMemories(prompt string, memories []index.Document) string {
	if len(memories) == 0 {
		return prompt
	}
	var sb strings.Builder
	sb.WriteString("Relevant past context:\n")
	for _, doc := range memories {
		sb.WriteString(doc.Content)
		sb.WriteString("\n---\n")
	}
	sb.WriteString(prompt)
	return sb.String()
}
