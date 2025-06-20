package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	log "github.com/sirupsen/logrus"

	// import config only for MemoryConfig type
	"github.com/adrianliechti/wingman/pkg/chain"
	"github.com/adrianliechti/wingman/pkg/provider"
	"github.com/adrianliechti/wingman/pkg/template"
	"github.com/adrianliechti/wingman/pkg/tool"

	"github.com/adrianliechti/wingman/pkg/memory"
	"github.com/google/uuid"
)

var _ chain.Provider = &Chain{}

type Chain struct {
	model string

	completer provider.Completer

	tools    []tool.Provider
	messages []provider.Message

	effort      provider.ReasoningEffort
	temperature *float32

	MemoryConfig *struct {
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

func New(model string, options ...Option) (*Chain, error) {
	c := &Chain{
		model: model,
	}

	for _, option := range options {
		option(c)
	}

	if c.completer == nil {
		return nil, errors.New("missing completer provider")
	}

	return c, nil
}

/* WithMemoryConfig should be implemented in the config package, not here, to avoid import cycles. */

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

func WithTools(tool ...tool.Provider) Option {
	return func(c *Chain) {
		c.tools = tool
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

	var contextFiles []provider.File

	for _, m := range messages {
		var files []provider.File

		for _, c := range m.Content {
			if c.File != nil {
				files = append(files, *c.File)
			}
		}

		contextFiles = files
	}

	if len(contextFiles) > 0 {
		ctx = tool.WithFiles(ctx, contextFiles)
	}

	input := slices.Clone(messages)

	agentTools := make(map[string]tool.Provider)
	inputTools := make(map[string]provider.Tool)

	for _, p := range c.tools {
		tools, err := p.Tools(ctx)

		if err != nil {
			return nil, err
		}

		for _, tool := range tools {
			agentTools[tool.Name] = p
			inputTools[tool.Name] = tool
		}
	}

	for _, t := range options.Tools {
		inputTools[t.Name] = t
	}

	inputOptions := &provider.CompleteOptions{
		Effort: options.Effort,

		Stop:  options.Stop,
		Tools: slices.Collect(maps.Values(inputTools)),

		MaxTokens:   options.MaxTokens,
		Temperature: options.Temperature,

		Format: options.Format,
		Schema: options.Schema,
	}

	acc := provider.CompletionAccumulator{}
	accID := uuid.New().String()

	var lastToolID string
	var lastToolName string

	stream := func(ctx context.Context, completion provider.Completion) error {
		acc.Add(completion)

		delta := provider.Completion{
			ID:    accID,
			Model: c.model,

			Reason: completion.Reason,

			Message: &provider.Message{
				Role: provider.MessageRoleAssistant,
			},

			Usage: completion.Usage,
		}

		for _, c := range completion.Message.Content {
			if c.Text != "" {
				delta.Message.Content = append(delta.Message.Content, provider.TextContent(c.Text))
			}

			if c.Refusal != "" {
				delta.Message.Content = append(delta.Message.Content, provider.RefusalContent(c.Text))
			}

			if c.ToolCall != nil {
				if c.ToolCall.ID != "" {
					lastToolID = c.ToolCall.ID
				}

				if c.ToolCall.Name != "" {
					lastToolName = c.ToolCall.Name
				}

				if lastToolName != "" {
					if _, found := agentTools[lastToolName]; found {
						continue
					}

					delta.Message.Content = append(delta.Message.Content, provider.ToolCallContent(provider.ToolCall{
						ID:   lastToolID,
						Name: lastToolName,

						Arguments: c.ToolCall.Arguments,
					}))
				}
			}
		}

		return options.Stream(ctx, delta)
	}

	if options.Stream != nil {
		inputOptions.Stream = stream
	}

	for {
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

		completion, err := c.completer.Complete(ctx, input, inputOptions)

		if err != nil {
			return nil, err
		}

		completion.ID = accID
		completion.Model = c.model

		if completion.Message == nil {
			return completion, nil
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
			log.Infof("[AgentChain] memoryManager nil? %v, userText: %q, assistantText: %q", memoryManager == nil, userText, assistantText)
			if userText != "" && assistantText != "" {
				err := memoryManager.LogTurn(ctx, userText, assistantText, nil, nil)
				if err != nil {
					log.Errorf("[AgentChain] LogTurn error: %v", err)
				} else {
					log.Info("[AgentChain] LogTurn called successfully")
				}
			}
		}

		var loop bool

		input = append(input, *completion.Message)

		for _, c := range completion.Message.Content {
			if c.ToolCall == nil {
				continue
			}

			t, found := agentTools[c.ToolCall.Name]

			if !found {
				continue
			}

			var params map[string]any

			if err := json.Unmarshal([]byte(c.ToolCall.Arguments), &params); err != nil {
				return nil, err
			}

			result, err := t.Execute(ctx, c.ToolCall.Name, params)

			if err != nil {
				return nil, err
			}

			data, err := json.Marshal(result)

			if err != nil {
				return nil, err
			}

			input = append(input, provider.Message{
				Role: provider.MessageRoleUser,

				Content: []provider.Content{
					provider.ToolResultContent(provider.ToolResult{
						ID:   c.ToolCall.ID,
						Data: string(data),
					}),
				},
			})

			loop = true
		}

		if !loop {
			return completion, nil
		}
	}
}
