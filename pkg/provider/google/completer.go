package google

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"github.com/adrianliechti/wingman/pkg/provider"
)

var _ provider.Completer = (*Completer)(nil)

type Completer struct {
	*Config
}

func NewCompleter(model string, options ...Option) (*Completer, error) {
	cfg := &Config{
		model: model,
	}

	for _, option := range options {
		option(cfg)
	}

	return &Completer{
		Config: cfg,
	}, nil
}

func (c *Completer) Complete(ctx context.Context, messages []provider.Message, options *provider.CompleteOptions) (*provider.Completion, error) {
	if options == nil {
		options = new(provider.CompleteOptions)
	}

	client, err := c.newClient(ctx)

	if err != nil {
		return nil, err
	}

	contents, err := convertMessages(messages)

	if err != nil {
		return nil, err
	}

	config := convertGenerateConfig(convertInstruction(messages), options)

	if options.Stream != nil {
		return c.completeStream(ctx, client, contents, config, options)
	}

	return c.complete(ctx, client, contents, config, options)
}

func (c *Completer) complete(ctx context.Context, client *genai.Client, contents []*genai.Content, config *genai.GenerateContentConfig, options *provider.CompleteOptions) (*provider.Completion, error) {
	resp, err := client.Models.GenerateContent(ctx, c.model, contents, config)

	if err != nil {
		return nil, convertError(err)
	}

	candidate := resp.Candidates[0]

	return &provider.Completion{
		ID:    uuid.New().String(),
		Model: c.model,

		Reason: toCompletionResult(candidate),

		Message: &provider.Message{
			Role:    provider.MessageRoleAssistant,
			Content: toContent(candidate.Content),
		},

		Usage: toCompletionUsage(resp.UsageMetadata),
	}, nil
}

func (c *Completer) completeStream(ctx context.Context, client *genai.Client, contents []*genai.Content, config *genai.GenerateContentConfig, options *provider.CompleteOptions) (*provider.Completion, error) {
	iter := client.Models.GenerateContentStream(ctx, c.model, contents, config)

	result := provider.CompletionAccumulator{}

	for resp, err := range iter {
		if err != nil {
			return nil, convertError(err)
		}

		delta := provider.Completion{
			ID: uuid.New().String(),

			Message: &provider.Message{
				Role: provider.MessageRoleAssistant,
			},

			Usage: toCompletionUsage(resp.UsageMetadata),
		}

		if len(resp.Candidates) > 0 {
			candidate := resp.Candidates[0]

			delta.Reason = toCompletionResult(candidate)
			delta.Message.Content = toContent(candidate.Content)
		}

		result.Add(delta)

		if err := options.Stream(ctx, delta); err != nil {
			return nil, err
		}
	}

	return result.Result(), nil
}

func convertInstruction(messages []provider.Message) *genai.Content {
	var parts []*genai.Part

	for _, m := range messages {
		if m.Role != provider.MessageRoleSystem {
			continue
		}

		for _, c := range m.Content {
			if c.Text != "" {
				parts = append(parts, genai.NewPartFromText(c.Text))
			}
		}
	}

	if len(parts) == 0 {
		return nil
	}

	return &genai.Content{
		Parts: parts,
	}
}

func convertGenerateConfig(instruction *genai.Content, options *provider.CompleteOptions) *genai.GenerateContentConfig {
	config := &genai.GenerateContentConfig{
		SystemInstruction: instruction,
	}

	if len(options.Tools) > 0 {
		config.Tools = convertTools(options.Tools)
	}

	if len(options.Stop) > 0 {
		config.StopSequences = options.Stop
	}

	if options.MaxTokens != nil {
		config.MaxOutputTokens = int32(*options.MaxTokens)
	}

	if options.Temperature != nil {
		config.Temperature = options.Temperature
	}

	if options.Format == provider.CompletionFormatJSON || options.Schema != nil {
		config.ResponseMIMEType = "application/json"

		if options.Schema != nil {
			config.ResponseJsonSchema = options.Schema.Schema
		}
	}

	return config
}

func convertContent(message provider.Message) (*genai.Content, error) {
	content := &genai.Content{}

	switch message.Role {
	case provider.MessageRoleUser:
		content.Role = "user"

		for _, c := range message.Content {
			if c.Text != "" {
				content.Parts = append(content.Parts, genai.NewPartFromText(c.Text))
			}

			if c.File != nil {
				switch c.File.ContentType {
				case "image/png", "image/jpeg", "image/webp", "image/heic", "image/heif":
					format := strings.Split(c.File.ContentType, "/")[1]

					part := genai.NewPartFromBytes(c.File.Content, format)
					content.Parts = append(content.Parts, part)

				default:
					return nil, errors.New("unsupported content type")
				}
			}

			if c.ToolResult != nil {
				var data any
				json.Unmarshal([]byte(c.ToolResult.Data), &data)

				var parameters map[string]any

				if val, ok := data.(map[string]any); ok {
					parameters = val
				}

				if val, ok := data.([]any); ok {
					parameters = map[string]any{"data": val}
				}

				part := genai.NewPartFromFunctionResponse(c.ToolResult.ID, parameters)
				content.Parts = append(content.Parts, part)
			}
		}

	case provider.MessageRoleAssistant:
		content.Role = "model"

		for _, c := range message.Content {
			if c.Text != "" {
				part := genai.NewPartFromText(c.Text)
				content.Parts = append(content.Parts, part)
			}

			if c.ToolCall != nil {
				var data map[string]any
				json.Unmarshal([]byte(c.ToolCall.Arguments), &data)

				part := genai.NewPartFromFunctionCall(c.ToolCall.Name, data)
				content.Parts = append(content.Parts, part)
			}
		}
	}

	return content, nil
}

func convertMessages(messages []provider.Message) ([]*genai.Content, error) {
	var result []*genai.Content

	for _, m := range messages {
		if m.Role == provider.MessageRoleUser {
			content, err := convertContent(m)

			if err != nil {
				return nil, err
			}

			result = append(result, content)
		}

		if m.Role == provider.MessageRoleAssistant {
			content, err := convertContent(m)

			if err != nil {
				return nil, err
			}

			result = append(result, content)
		}
	}

	return result, nil
}

func convertTools(tools []provider.Tool) []*genai.Tool {
	var functions []*genai.FunctionDeclaration

	for _, t := range tools {
		function := &genai.FunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,

			ParametersJsonSchema: t.Parameters,
		}

		functions = append(functions, function)
	}

	if len(functions) == 0 {
		return nil
	}

	return []*genai.Tool{
		{
			FunctionDeclarations: functions,
		},
	}
}

func toContent(content *genai.Content) []provider.Content {
	var parts []provider.Content

	for _, p := range content.Parts {
		if p.Text != "" {
			parts = append(parts, provider.TextContent(p.Text))
		}

		if p.FunctionCall != nil {
			data, _ := json.Marshal(p.FunctionCall.Args)

			call := provider.ToolCall{
				ID: uuid.NewString(),

				Name:      p.FunctionCall.Name,
				Arguments: string(data),
			}

			parts = append(parts, provider.ToolCallContent(call))
		}
	}

	return parts
}

func toCompletionResult(candidate *genai.Candidate) provider.CompletionReason {
	if candidate.Content != nil {
		for _, p := range candidate.Content.Parts {
			if p.FunctionCall != nil {
				return provider.CompletionReasonTool
			}
		}
	}

	switch candidate.FinishReason {
	case genai.FinishReasonStop:
		return provider.CompletionReasonStop

	case genai.FinishReasonMaxTokens:
		return provider.CompletionReasonLength

	case genai.FinishReasonSafety:
		return provider.CompletionReasonFilter

	case genai.FinishReasonRecitation:
		return provider.CompletionReasonFilter
	}

	return ""
}

func toCompletionUsage(metadata *genai.GenerateContentResponseUsageMetadata) *provider.Usage {
	if metadata == nil {
		return nil
	}

	return &provider.Usage{
		InputTokens:  int(metadata.PromptTokenCount),
		OutputTokens: int(metadata.CandidatesTokenCount),
	}
}
