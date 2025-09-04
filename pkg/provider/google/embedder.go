package google

import (
	"context"

	"github.com/adrianliechti/wingman/pkg/provider"

	"google.golang.org/genai"
)

var _ provider.Embedder = (*Embedder)(nil)

type Embedder struct {
	*Config
}

func NewEmbedder(model string, options ...Option) (*Embedder, error) {
	cfg := &Config{
		model: model,
	}

	for _, option := range options {
		option(cfg)
	}

	return &Embedder{
		Config: cfg,
	}, nil
}

func (e *Embedder) Embed(ctx context.Context, texts []string) (*provider.Embedding, error) {
	client, err := e.newClient(ctx)

	if err != nil {
		return nil, err
	}

	var contents []*genai.Content

	resp, err := client.Models.EmbedContent(ctx, e.model, contents, nil)

	if err != nil {
		return nil, convertError(err)
	}

	result := &provider.Embedding{
		Model: e.model,
	}

	for _, e := range resp.Embeddings {
		result.Embeddings = append(result.Embeddings, e.Values)
	}

	return result, nil
}
