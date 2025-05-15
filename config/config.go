package config

import (
	"bytes"
	"errors"
	"os"

	"github.com/adrianliechti/wingman/pkg/api"
	"github.com/adrianliechti/wingman/pkg/authorizer"
	"github.com/adrianliechti/wingman/pkg/chain"
	"github.com/adrianliechti/wingman/pkg/connector"
	"github.com/adrianliechti/wingman/pkg/extractor"
	"github.com/adrianliechti/wingman/pkg/index"
	"github.com/adrianliechti/wingman/pkg/provider"
	"github.com/adrianliechti/wingman/pkg/segmenter"
	"github.com/adrianliechti/wingman/pkg/summarizer"
	"github.com/adrianliechti/wingman/pkg/tool"
	"github.com/adrianliechti/wingman/pkg/translator"

	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Address string

	Authorizers []authorizer.Provider

	models map[string]provider.Model

	completer   map[string]provider.Completer
	embedder    map[string]provider.Embedder
	renderer    map[string]provider.Renderer
	reranker    map[string]provider.Reranker
	synthesizer map[string]provider.Synthesizer
	transcriber map[string]provider.Transcriber

	indexes map[string]index.Provider

	extractors map[string]extractor.Provider
	segmenter  map[string]segmenter.Provider
	summarizer map[string]summarizer.Provider
	translator map[string]translator.Provider

	tools  map[string]tool.Provider
	chains map[string]chain.Provider

	connectors map[string]connector.Provider

	APIs map[string]api.Provider
}

func (c *Config) RegisterConnector(id string, p connector.Provider) {
	if c.connectors == nil {
		c.connectors = make(map[string]connector.Provider)
	}

	c.connectors[id] = p
}

func (c *Config) Connector(id string) (connector.Provider, error) {
	if c.connectors != nil {
		if p, ok := c.connectors[id]; ok {
			return p, nil
		}
	}

	return nil, errors.New("connector not found: " + id)
}

// AllConnectors returns a map of all registered connectors.
// It returns a copy to prevent external modification of the internal map.
func (c *Config) AllConnectors() map[string]connector.Provider {
	if c.connectors == nil {
		return make(map[string]connector.Provider)
	}

	// Return a copy
	connectorsCopy := make(map[string]connector.Provider, len(c.connectors))
	for id, p := range c.connectors {
		connectorsCopy[id] = p
	}
	return connectorsCopy
}

func Parse(path string) (*Config, error) {
	file, err := parseFile(path)

	if err != nil {
		return nil, err
	}

	c := &Config{
		Address: ":8080",
	}

	if err := c.registerAuthorizer(file); err != nil {
		return nil, err
	}

	if err := c.registerProviders(file); err != nil {
		return nil, err
	}

	if err := c.registerExtractors(file); err != nil {
		return nil, err
	}

	if err := c.registerSegmenters(file); err != nil {
		return nil, err
	}

	if err := c.registerSummarizers(file); err != nil {
		return nil, err
	}

	if err := c.registerTranslators(file); err != nil {
		return nil, err
	}

	if err := c.registerIndexes(file); err != nil {
		return nil, err
	}

	if err := c.registerTools(file); err != nil {
		return nil, err
	}

	if err := c.registerRouters(file); err != nil {
		return nil, err
	}

	if err := c.registerChains(file); err != nil {
		return nil, err
	}

	if err := c.registerConnectors(file); err != nil {
		return nil, err
	}

	if err := c.registerAPI(file); err != nil {
		return nil, err
	}

	return c, nil
}

type configFile struct {
	Authorizers []authorizerConfig `yaml:"authorizers"`

	Providers []providerConfig `yaml:"providers"`

	Indexes yaml.Node `yaml:"indexes"`

	Extractors  yaml.Node `yaml:"extractors"`
	Segmenters  yaml.Node `yaml:"segmenters"`
	Translators yaml.Node `yaml:"translators"`

	Tools  yaml.Node `yaml:"tools"`
	Chains yaml.Node `yaml:"chains"`

	Routers yaml.Node `yaml:"routers"`

	Connectors yaml.Node `yaml:"connectors,omitempty"`

	APIs yaml.Node `yaml:"apis"`
}

func parseFile(path string) (*configFile, error) {
	data, err := os.ReadFile(path)

	if err != nil {
		return nil, err
	}

	data = []byte(os.ExpandEnv(string(data)))

	var config configFile

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

func createLimiter(limit *int) *rate.Limiter {
	if limit == nil {
		return nil
	}

	return rate.NewLimiter(rate.Limit(*limit), *limit)
}

func parseEffort(val string) provider.ReasoningEffort {
	switch val {
	case string(provider.ReasoningEffortLow):
		return provider.ReasoningEffortLow

	case string(provider.ReasoningEffortMedium):
		return provider.ReasoningEffortMedium

	case string(provider.ReasoningEffortHigh):
		return provider.ReasoningEffortHigh
	}

	return ""
}
