package provider

import (
	"context"
	"io"
)

type Renderer interface {
	Render(ctx context.Context, input string, options *RenderOptions) (*Image, error)
}

type RenderOptions struct {
	Images []File
}

type Image struct {
	ID string

	Name   string
	Reader io.ReadCloser
}
