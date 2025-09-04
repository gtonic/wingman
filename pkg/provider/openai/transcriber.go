package openai

import (
	"bytes"
	"context"

	"github.com/adrianliechti/wingman/pkg/provider"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v2"
)

var _ provider.Transcriber = (*Transcriber)(nil)

type Transcriber struct {
	*Config
	transcriptions openai.AudioTranscriptionService
}

func NewTranscriber(url, model string, options ...Option) (*Transcriber, error) {
	cfg := &Config{
		url:   url,
		model: model,
	}

	for _, option := range options {
		option(cfg)
	}

	return &Transcriber{
		Config:         cfg,
		transcriptions: openai.NewAudioTranscriptionService(cfg.HackOldAzure()...),
	}, nil
}

func (t *Transcriber) Transcribe(ctx context.Context, input provider.File, options *provider.TranscribeOptions) (*provider.Transcription, error) {
	if options == nil {
		options = new(provider.TranscribeOptions)
	}

	id := uuid.NewString()

	transcription, err := t.transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		Model: t.model,

		File: openai.File(bytes.NewReader(input.Content), input.Name, input.ContentType),

		ResponseFormat: openai.AudioResponseFormatJSON,
	})

	if err != nil {
		return nil, convertError(err)
	}

	result := provider.Transcription{
		ID:    id,
		Model: t.model,

		Text: transcription.Text,
	}

	// var metadata struct {
	// 	Language string  `json:"language"`
	// 	Duration float64 `json:"duration"`
	// }

	// if err := json.Unmarshal([]byte(transcription.RawJSON()), &metadata); err == nil {
	// 	result.Language = metadata.Language
	// 	result.Duration = metadata.Duration
	// }

	return &result, nil
}
