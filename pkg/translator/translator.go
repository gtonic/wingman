package translator

import (
	"context"
	"errors"

	"github.com/adrianliechti/wingman/pkg/provider"
)

type Provider interface {
	Translate(ctx context.Context, input Input, options *TranslateOptions) (*provider.File, error)
}

var (
	ErrUnsupported = errors.New("unsupported type")
)

type TranslateOptions struct {
	Language string
}

type Input struct {
	Text string

	File *provider.File
}
