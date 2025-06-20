package config

import (
	"fmt"
	"time"

	"github.com/adrianliechti/wingman/pkg/connector"
	"github.com/adrianliechti/wingman/pkg/connector/signal"
)

type connectorConfig struct {
	Type string `yaml:"type"`

	// For Signal Connector
	URL                  string `yaml:"url,omitempty"`
	AccountNumber        string `yaml:"account_number,omitempty"`
	Completer            string `yaml:"completer,omitempty"`
	PollInterval         string `yaml:"poll_interval,omitempty"`          // e.g., "5s", "1m", used for poll mode
	ReceiveMode          string `yaml:"receive_mode,omitempty"`           // "poll" or "websocket" (defaults to "poll")
	MaxHistoryMessages   int    `yaml:"max_history_messages,omitempty"`   // Max messages (user + assistant) to keep in history (e.g., 20 for 10 turns)
	HistoryStorageType   string `yaml:"history_storage_type,omitempty"`   // "memory" (defaults to "memory")
	MessagePrefixTrigger string `yaml:"message_prefix_trigger,omitempty"` // Optional prefix to trigger LLM processing (e.g., "Q")
	AccountUsername      string `yaml:"account_username,omitempty"`       // Optional: Bot's Signal Username (e.g., name.01) to use as sender for group messages
	LLMTimeoutSeconds    int    `yaml:"llm_timeout_seconds,omitempty"`    // Timeout for LLM completion in seconds
}

func (c *Config) registerConnectors(f *configFile) error {
	var configs map[string]connectorConfig

	if err := f.Connectors.Decode(&configs); err != nil {
		// If Connectors is not a map, it might be empty or malformed,
		// which can be okay if no connectors are defined.
		// However, if there's content that fails to decode into a map, it's an error.
		if f.Connectors.Content != nil && len(f.Connectors.Content) > 0 {
			return fmt.Errorf("failed to decode connectors: %w", err)
		}
		return nil // No connectors defined or empty section
	}

	if c.connectors == nil {
		c.connectors = make(map[string]connector.Provider)
	}
	for id, cfg := range configs {
		var p connector.Provider
		var err error

		switch cfg.Type {
		case "signal":
			p, err = createSignalConnector(id, cfg, c)
		default:
			err = fmt.Errorf("unknown connector type: %s for id: %s", cfg.Type, id)
		}

		if err != nil {
			return fmt.Errorf("failed to create connector %s (type %s): %w", id, cfg.Type, err)
		}

		if p != nil {
			c.connectors[id] = p
		}
	}

	return nil
}

// createSignalConnector is a placeholder. Implementation will be in pkg/connector/signal.
func createSignalConnector(id string, cfg connectorConfig, appConfig *Config) (connector.Provider, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("signal connector %s: url is required", id)
	}

	if cfg.AccountNumber == "" {
		return nil, fmt.Errorf("signal connector %s: account_number is required", id)
	}

	if cfg.Completer == "" {
		return nil, fmt.Errorf("signal connector %s: completer is required", id)
	}

	completer, err := appConfig.Completer(cfg.Completer)
	if err != nil {
		return nil, fmt.Errorf("signal connector %s: failed to get completer '%s': %w", id, cfg.Completer, err)
	}

	pollInterval := 5 * time.Second // Default poll interval
	if cfg.PollInterval != "" {
		parsedInterval, err := time.ParseDuration(cfg.PollInterval)
		if err != nil {
			return nil, fmt.Errorf("signal connector %s: invalid poll_interval '%s': %w", id, cfg.PollInterval, err)
		}
		pollInterval = parsedInterval
	}

	signalCfg := signal.Config{
		URL:                  cfg.URL,
		AccountNumber:        cfg.AccountNumber,
		AccountUsername:      cfg.AccountUsername,
		Completer:            completer,
		PollInterval:         pollInterval,
		ReceiveMode:          cfg.ReceiveMode,
		MessagePrefixTrigger: cfg.MessagePrefixTrigger,
		LLMTimeoutSeconds:    cfg.LLMTimeoutSeconds,
	}

	return signal.New(id, signalCfg)
}
