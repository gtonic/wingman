package assistant

import (
	"context"
	"errors"
	"log"
	"slices"

	"github.com/adrianliechti/wingman/pkg/chain"
	"github.com/adrianliechti/wingman/pkg/memory"
	"github.com/adrianliechti/wingman/pkg/provider"
	"github.com/adrianliechti/wingman/pkg/template"
)

var _ chain.Provider = &Chain{}

type Chain struct {
	completer provider.Completer

	messages []provider.Message

	effort      provider.ReasoningEffort
	temperature *float32

	MemoryConfig *struct {
		Enabled          bool
		Index            string
		RecallK          int
		LogConversations bool
		InjectMemories   bool
	}
	memoryProvider memory.MemoryProvider
}

type Option func(*Chain)

func WithMemoryProvider(provider memory.MemoryProvider) Option {
	return func(c *Chain) {
		c.memoryProvider = provider
	}
}

func New(options ...Option) (*Chain, error) {
	c := &Chain{}

	for _, option := range options {
		option(c)
	}

	if c.completer == nil {
		return nil, errors.New("missing completer provider")
	}

	return c, nil
}

func WithCompleter(completer provider.Completer) Option {
	return func(c *Chain) {
		c.completer = completer
	}
}

func WithMessages(messages ...provider.Message) Option {
	return func(c *Chain) {
		c.messages = messages
	}
}

func WithEffort(effort provider.ReasoningEffort) Option {
	return func(c *Chain) {
		c.effort = effort
	}
}

func WithTemperature(temperature float32) Option {
	return func(c *Chain) {
		c.temperature = &temperature
	}
}

func (c *Chain) Complete(ctx context.Context, messages []provider.Message, options *provider.CompleteOptions) (*provider.Completion, error) {
	log.Printf("[DEBUG] Entered Chain.Complete for chain with memory config: %+v", c.MemoryConfig)
	var memoryManager *memory.MemoryManager
	if c.MemoryConfig != nil && c.memoryProvider != nil {
		memCfg := &memory.MemoryConfig{
			Index:            c.MemoryConfig.Index,
			RecallK:          c.MemoryConfig.RecallK,
			LogConversations: c.MemoryConfig.LogConversations,
			InjectMemories:   c.MemoryConfig.InjectMemories,
		}
		memoryManager = memory.NewMemoryManager(memCfg, c.memoryProvider)
	}

	if options == nil {
		options = new(provider.CompleteOptions)
	}

	if options.Effort == "" {
		options.Effort = c.effort
	}

	if options.Temperature == nil {
		options.Temperature = c.temperature
	}

	if len(c.messages) > 0 {
		values, err := template.Messages(c.messages, nil)

		if err != nil {
			return nil, err
		}

		messages = slices.Concat(values, messages)
	}

	input := slices.Clone(messages)

	// Inject memories if enabled
	if memoryManager != nil && c.MemoryConfig.InjectMemories {
		// Use the last user message as the query
		var lastUser string
		for i := len(input) - 1; i >= 0; i-- {
			if input[i].Role == provider.MessageRoleUser {
				if len(input[i].Content) > 0 && input[i].Content[0].Text != "" {
					lastUser = input[i].Content[0].Text
					break
				}
			}
		}
		if lastUser != "" {
			memories, _ := memoryManager.RecallMemories(ctx, lastUser, nil)
			if len(memories) > 0 {
				// Prepend as a system message
				memText := memory.InjectMemories("", memories)
				input = append([]provider.Message{{
					Role:    provider.MessageRoleSystem,
					Content: []provider.Content{provider.TextContent(memText)},
				}}, input...)
			}
		}
	}

	completion, err := c.completer.Complete(ctx, input, options)
	if err != nil {
		return nil, err
	}

	// Log conversation turn if enabled
	if memoryManager != nil && c.MemoryConfig.LogConversations {
		var userText, assistantText string
		// Find last user message
		for i := len(input) - 1; i >= 0; i-- {
			if input[i].Role == provider.MessageRoleUser {
				if len(input[i].Content) > 0 && input[i].Content[0].Text != "" {
					userText = input[i].Content[0].Text
					break
				}
			}
		}
		// Get assistant response
		if completion.Message != nil && len(completion.Message.Content) > 0 && completion.Message.Content[0].Text != "" {
			assistantText = completion.Message.Content[0].Text
		}
		if userText != "" && assistantText != "" {
			_ = memoryManager.LogTurn(ctx, userText, assistantText, nil, nil)
		}
	}

	return completion, nil
}
