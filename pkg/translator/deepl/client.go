package deepl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/adrianliechti/wingman/pkg/provider"
	"github.com/adrianliechti/wingman/pkg/translator"
)

var (
	_ translator.Provider = (*Client)(nil)
)

type Client struct {
	client *http.Client

	url   string
	token string
}

func New(url string, options ...Option) (*Client, error) {
	if url == "" {
		url = "https://api-free.deepl.com"
	}

	c := &Client{
		client: http.DefaultClient,

		url: url,
	}

	for _, option := range options {
		option(c)
	}

	return c, nil
}

func (c *Client) Translate(ctx context.Context, input translator.Input, options *translator.TranslateOptions) (*provider.File, error) {
	if options == nil {
		options = new(translator.TranslateOptions)
	}

	if options.Language == "" {
		options.Language = "en"
	}

	if input.File != nil {
		return nil, translator.ErrUnsupported
	}

	type bodyType struct {
		Text       []string `json:"text"`
		TargetLang string   `json:"target_lang"`
	}

	body := bodyType{
		Text: []string{
			strings.TrimSpace(input.Text),
		},

		TargetLang: options.Language,
	}

	u, _ := url.JoinPath(c.url, "/v2/translate")
	r, _ := http.NewRequestWithContext(ctx, "POST", u, jsonReader(body))
	r.Header.Add("Authorization", "DeepL-Auth-Key "+c.token)
	r.Header.Add("Content-Type", "application/json")

	resp, err := c.client.Do(r)

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, convertError(resp)
	}

	type resultType struct {
		Translations []struct {
			DetectedSourceLanguage string `json:"detected_source_language"`
			Text                   string `json:"text"`
		} `json:"translations"`
	}

	var result resultType

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Translations) == 0 {
		return nil, errors.New("unable to translate content")
	}

	return &provider.File{
		Content:     []byte(result.Translations[0].Text),
		ContentType: "text/plain",
	}, nil
}
