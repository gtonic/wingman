package config

import (
	"errors"
	"maps"
	"slices"

	"github.com/adrianliechti/wingman/pkg/mcp"
	"github.com/adrianliechti/wingman/pkg/tool"
)

func (cfg *Config) RegisterMCP(id string, s *mcp.Server) {
	if cfg.mcps == nil {
		cfg.mcps = make(map[string]*mcp.Server)
	}

	cfg.mcps[id] = s
}

func (cfg *Config) MCP(id string) (*mcp.Server, error) {
	if cfg.mcps != nil {
		if s, ok := cfg.mcps[id]; ok {
			return s, nil
		}
	}

	return nil, errors.New("mcp not found: " + id)
}

type mcpConfig struct {
	Name string `yaml:"name"`

	Tools []string `yaml:"tools"`

	Instructions string `yaml:"instructions"`
}

type mcpContext struct {
	Tools map[string]tool.Provider
}

func (cfg *Config) registerMCP(f *configFile) error {
	var configs map[string]mcpConfig

	if err := f.MCPs.Decode(&configs); err != nil {
		return err
	}

	for _, node := range f.MCPs.Content {
		id := node.Value

		config, ok := configs[node.Value]

		if !ok {
			continue
		}

		context := mcpContext{
			Tools: make(map[string]tool.Provider),
		}

		for _, t := range config.Tools {
			tool, err := cfg.Tool(t)

			if err != nil {
				return err
			}

			context.Tools[t] = tool
		}

		mcp, err := createMCP(config, context)

		if err != nil {
			return err
		}

		cfg.RegisterMCP(id, mcp)
	}

	return nil
}

func createMCP(config mcpConfig, context mcpContext) (*mcp.Server, error) {
	name := config.Name

	if config.Name != "" {
		name = config.Name
	}

	tools := slices.Collect(maps.Values(context.Tools))

	return mcp.New(name, tools)
}
