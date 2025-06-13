package llm

import (
	"context"

	"github.com/adrianliechti/wingman/pkg/provider"
	"github.com/adrianliechti/wingman/pkg/translator"
)

type Client struct {
	completer provider.Completer
}

func New(completer provider.Completer) (*Client, error) {
	c := &Client{
		completer: completer,
	}

	return c, nil
}

func (c *Client) Translate(ctx context.Context, input translator.Input, options *translator.TranslateOptions) (*provider.File, error) {
	if options == nil {
		options = new(translator.TranslateOptions)
	}

	if input.File != nil {
		return nil, translator.ErrUnsupported
	}

	messages := []provider.Message{
		provider.SystemMessage("Act as a translator. Translate the following text to `" + options.Language + "`. Only return the translation, no other text."),
		provider.UserMessage(input.Text),
	}

	completion, err := c.completer.Complete(ctx, messages, nil)

	if err != nil {
		return nil, err
	}

	return &provider.File{
		Content:     []byte(completion.Message.Text()),
		ContentType: "text/plain",
	}, nil
}
