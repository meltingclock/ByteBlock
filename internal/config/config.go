package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	WSS_URL          string `yaml:"WSS_URL"`
	HTTPS_URL        string `yaml:"HTTPS_URL"`
	BOT_ADDRESS      string `yaml:"BOT_ADDRESS"`
	PRIVATE_KEY      string `yaml:"PRIVATE_KEY"`
	IDENTITY_KEY     string `yaml:"IDENTITY_KEY"`
	TELEGRAM_TOKEN   string `yaml:"TELEGRAM_TOKEN"`
	TELEGRAM_CHAT_ID int64  `yaml:"TELEGRAM_CHAT_ID"`

	// Ignored for now
	USE_ALERT bool `yaml:"USE_ALERT"`
	DEBUG     bool `yaml:"DEBUG"`
}

const DefaultPath = "config.yml"

func Default() *Config {
	return &Config{
		WSS_URL:          "",
		HTTPS_URL:        "",
		BOT_ADDRESS:      "",
		PRIVATE_KEY:      "",
		IDENTITY_KEY:     "",
		TELEGRAM_TOKEN:   "",
		TELEGRAM_CHAT_ID: 0,
		USE_ALERT:        false,
		DEBUG:            true,
	}
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	// create if missing
	if _, err := os.Stat(path); os.IsNotExist(err) {
		cfg := Default()
		if err := Save(path, cfg); err != nil {
			return nil, fmt.Errorf("create default config: %w", err)
		}
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func Save(path string, cfg *Config) error {
	if path == "" {
		path = DefaultPath
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}
