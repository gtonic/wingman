package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/adrianliechti/wingman/pkg/provider"
	"github.com/google/uuid"

	"github.com/openai/openai-go"
)

var _ provider.Renderer = (*Renderer)(nil)

type Renderer struct {
	*Config
	images openai.ImageService
}

func NewRenderer(url, model string, options ...Option) (*Renderer, error) {
	cfg := &Config{
		url:   url,
		model: model,
	}

	for _, option := range options {
		option(cfg)
	}

	return &Renderer{
		Config: cfg,
		images: openai.NewImageService(cfg.Options()...),
	}, nil
}

func (r *Renderer) Render(ctx context.Context, input string, options *provider.RenderOptions) (*provider.Image, error) {
	if options == nil {
		options = new(provider.RenderOptions)
	}

	image, err := r.images.Generate(ctx, openai.ImageGenerateParams{
		Model:  r.model,
		Prompt: input,
	})

	if err != nil {
		return nil, convertError(err)
	}

	data, err := r.getData(ctx, image.Data[0])

	if err != nil {
		return nil, err
	}

	id := uuid.NewString()

	return &provider.Image{
		ID:   id,
		Name: id + ".png",

		Reader: io.NopCloser(bytes.NewReader(data)),
	}, nil
}

func (r *Renderer) getData(ctx context.Context, image openai.Image) ([]byte, error) {
	if image.URL != "" {
		if strings.HasPrefix(image.URL, "data:") {
			re := regexp.MustCompile(`data:([a-zA-Z]+\/[a-zA-Z0-9.+_-]+);base64,\s*(.+)`)

			match := re.FindStringSubmatch(image.URL)

			if len(match) != 3 {
				return nil, fmt.Errorf("invalid data url")
			}

			return base64.StdEncoding.DecodeString(match[2])
		}

		req, err := http.NewRequestWithContext(ctx, "GET", image.URL, nil)

		if err != nil {
			return nil, err
		}

		resp, err := r.client.Do(req)

		if err != nil {
			return nil, err
		}

		defer resp.Body.Close()

		return io.ReadAll(resp.Body)
	}

	if image.B64JSON != "" {
		return base64.StdEncoding.DecodeString(image.B64JSON)
	}

	return nil, errors.New("invalid image data")
}
