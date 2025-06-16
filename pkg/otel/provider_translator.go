package otel

import (
	"context"
	"strings"

	"github.com/adrianliechti/wingman/pkg/provider"
	"github.com/adrianliechti/wingman/pkg/translator"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type Translator interface {
	Observable
	translator.Provider
}

type observableTranslator struct {
	name    string
	library string

	model    string
	provider string

	translator translator.Provider
}

func NewTranslator(provider, model string, p translator.Provider) Translator {
	library := strings.ToLower(provider)

	return &observableTranslator{
		translator: p,

		name:    strings.TrimSuffix(strings.ToLower(provider), "-translator") + "-translator",
		library: library,

		model:    model,
		provider: provider,
	}
}

func (p *observableTranslator) otelSetup() {
}

func (p *observableTranslator) Translate(ctx context.Context, input translator.Input, options *translator.TranslateOptions) (*provider.File, error) {
	ctx, span := otel.Tracer(p.library).Start(ctx, p.name)
	defer span.End()

	result, err := p.translator.Translate(ctx, input, options)

	meterRequest(ctx, p.library, p.provider, "translate", p.model)

	if EnableDebug {
		inputText := ""
		outputText := ""

		if input.Text != "" {
			inputText = input.Text
		}

		if input.File != nil {
			inputText = input.File.Name
		}

		if result != nil && strings.HasPrefix(result.ContentType, "text/") {
			outputText = string(result.Content)
		}

		if inputText == "" {
			span.SetAttributes(attribute.String("input", inputText))
		}

		if outputText != "" {
			span.SetAttributes(attribute.String("output", outputText))
		}
	}

	return result, err
}
