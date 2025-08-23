package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	TELEGRAM_TOKEN   string `yaml:"TELEGRAM_TOKEN"`
	TELEGRAM_CHAT_ID int64  `yaml:"TELEGRAM_CHAT_ID"`

	// secrets kept in YAML (NOT via telegram)
	BOT_ADDRESS  string `yaml:"BOT_ADDRESS"`
	PRIVATE_KEY  string `yaml:"PRIVATE_KEY"`
	IDENTITY_KEY string `yaml:"IDENTITY_KEY"`

	// Ignored for now
	USE_ALERT bool `yaml:"USE_ALERT"`
	DEBUG     bool `yaml:"DEBUG"`
}

const DefaultPath = "config.yml"

func Default() *Config {
	return &Config{
		TELEGRAM_TOKEN:   "",
		TELEGRAM_CHAT_ID: 0,

		BOT_ADDRESS:  "",
		PRIVATE_KEY:  "",
		IDENTITY_KEY: "",

		USE_ALERT: false,
		DEBUG:     true,
	}
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("TELEGRAM_TOKEN"); v != "" {
		c.TELEGRAM_TOKEN = v
	}
	if v := os.Getenv("TELEGRAM_CHAT_ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.TELEGRAM_CHAT_ID = id
		}
	}
	if v := os.Getenv("BOT_ADDRESS"); v != "" {
		c.BOT_ADDRESS = v
	}
	if v := os.Getenv("PRIVATE_KEY"); v != "" {
		c.PRIVATE_KEY = v
	}
	if v := os.Getenv("IDENTITY_KEY"); v != "" {
		c.IDENTITY_KEY = v
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
	cfg.applyEnvOverrides()
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.TELEGRAM_TOKEN == "" {
		return fmt.Errorf("TELEGRAM_TOKEN is required (set in config.yml or TELEGRAM_TOKEN env)")
	}
	// You can enforce TELEGRAM_CHAT_ID != 0 if you want strict access.
	return nil
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
