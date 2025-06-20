// Package config: memory configuration for long-term memory integration.
package config

type MemoryConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Index            string `yaml:"index"`             // Which index to use for memory (e.g. "memory")
	RecallK          int    `yaml:"recall_k"`          // How many memories to recall per prompt
	LogConversations bool   `yaml:"log_conversations"` // Whether to log all chat turns
	InjectMemories   bool   `yaml:"inject_memories"`   // Whether to inject memories into LLM prompt
}
