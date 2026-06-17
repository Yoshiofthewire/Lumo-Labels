package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Paths struct {
	ConfigFile string
	StateDir   string
	LogDir     string
}

type Config struct {
	Timezone string `yaml:"timezone"`
	LogLevel string `yaml:"logLevel"`

	Llama struct {
		BaseURL      string `yaml:"baseUrl"`
		APIKey       string `yaml:"apiKey"`
		ClassifyPath string `yaml:"classifyPath"`
	} `yaml:"llama"`

	Scan struct {
		IntervalSeconds int `yaml:"intervalSeconds"`
	} `yaml:"scan"`

	RateLimits struct {
		PerMinute int `yaml:"perMinute"`
		PerHour   int `yaml:"perHour"`
	} `yaml:"rateLimits"`

	Redaction struct {
		Patterns []Pattern `yaml:"patterns"`
	} `yaml:"redaction"`

	Labels struct {
		Allowlist []string `yaml:"allowlist"`
	} `yaml:"labels"`
}

type Pattern struct {
	Name        string `yaml:"name"`
	Regex       string `yaml:"regex"`
	Replacement string `yaml:"replacement"`
}

func Default() Config {
	cfg := Config{
		Timezone: "America/New_York",
		LogLevel: "info",
	}
	cfg.Llama.BaseURL = "http://127.0.0.1:3333"
	cfg.Llama.APIKey = ""
	cfg.Llama.ClassifyPath = "/"
	cfg.Scan.IntervalSeconds = 90
	cfg.RateLimits.PerMinute = 10
	cfg.RateLimits.PerHour = 20
	cfg.Redaction.Patterns = []Pattern{
		{Name: "email", Regex: `(?i)\\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\\.[A-Z]{2,}\\b`, Replacement: "[REDACTED_EMAIL]"},
		{Name: "phone", Regex: `\\b(?:\\+?\\d{1,3}[\\s.-]?)?(?:\\(\\d{3}\\)|\\d{3})[\\s.-]?\\d{3}[\\s.-]?\\d{4}\\b`, Replacement: "[REDACTED_PHONE]"},
		{Name: "ssn", Regex: `\\b\\d{3}-\\d{2}-\\d{4}\\b`, Replacement: "[REDACTED_SSN]"},
		{Name: "iban", Regex: `\\b[A-Z]{2}\\d{2}[A-Z0-9]{10,30}\\b`, Replacement: "[REDACTED_IBAN]"},
		{Name: "card", Regex: `\\b(?:\\d[ -]*?){13,19}\\b`, Replacement: "[REDACTED_CARD]"},
	}
	return cfg
}

func LoadOrInit(path string) (Config, error) {
	if _, err := os.Stat(path); err == nil {
		return Load(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Config{}, fmt.Errorf("mkdir config dir: %w", err)
	}
	cfg := Default()
	if err := Save(path, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
