package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultRelativePath = ".config/murtaugh/slack.yaml"

type Config struct {
	BaseDir       string                        `yaml:"-"`
	Slack         SlackConfig                   `yaml:"slack"`
	ACP           ACPConfig                     `yaml:"acp"`
	Commands      []CommandConfig               `yaml:"commands"`
	WorkflowRules map[string]WorkflowRuleConfig `yaml:"workflow-rules"`
}

type SlackConfig struct {
	AppToken  string `yaml:"app_token"`
	BotToken  string `yaml:"bot_token"`
	AdminUser string `yaml:"admin_user"`
	Debug     bool   `yaml:"debug"`
}

type CommandConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type ACPConfig struct {
	Enabled              bool     `yaml:"enabled"`
	Command              string   `yaml:"command"`
	Args                 []string `yaml:"args"`
	WorkDir              string   `yaml:"workdir"`
	StartupTimeout       string   `yaml:"startup_timeout"`
	RequestTimeout       string   `yaml:"request_timeout"`
	SessionIdleTimeout   string   `yaml:"session_idle_timeout"`
	MaxSessions          int      `yaml:"max_sessions"`
	StreamAppendInterval string   `yaml:"stream_append_interval"`
	StreamMinChunkChars  int      `yaml:"stream_min_chunk_chars"`
	StreamFinalFeedback  bool     `yaml:"stream_final_feedback"`
}

type WorkflowRuleConfig struct {
	RequestEvent string          `yaml:"request_event"`
	Match        map[string]any  `yaml:"match"`
	Triggers     []TriggerConfig `yaml:"trigger"`
}

type TriggerConfig struct {
	Type         string
	ReplyToSlack *ReplyToSlackTriggerConfig
	Run          *RunTriggerConfig
}

type ReplyToSlackTriggerConfig struct {
	Template string            `yaml:"template"`
	Run      *RunTriggerConfig `yaml:"run"`
}

type RunTriggerConfig struct {
	Cmd     string   `yaml:"cmd"`
	Args    []string `yaml:"args"`
	Timeout string   `yaml:"timeout"`
	WorkDir string   `yaml:"workdir"`
}

func (t *TriggerConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode || len(value.Content) != 2 {
		return errors.New("trigger must be a mapping with exactly one action")
	}

	action := value.Content[0].Value
	switch action {
	case "reply-to-slack":
		var cfg ReplyToSlackTriggerConfig
		if err := value.Content[1].Decode(&cfg); err != nil {
			return err
		}
		t.Type = action
		t.ReplyToSlack = &cfg
	case "run":
		var cfg RunTriggerConfig
		if err := value.Content[1].Decode(&cfg); err != nil {
			return err
		}
		t.Type = action
		t.Run = &cfg
	default:
		return fmt.Errorf("unsupported trigger action %q", action)
	}
	return nil
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, defaultRelativePath), nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseDir = filepath.Dir(path)
	return cfg, nil
}

func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Slack.AppToken) == "" {
		errs = append(errs, errors.New("slack.app_token is required"))
	}
	if strings.TrimSpace(c.Slack.BotToken) == "" {
		errs = append(errs, errors.New("slack.bot_token is required"))
	}
	for i, command := range c.Commands {
		if !strings.HasPrefix(strings.TrimSpace(command.Name), "/") {
			errs = append(errs, fmt.Errorf("commands[%d].name must start with /", i))
		}
	}
	if err := c.ACP.Validate(); err != nil {
		errs = append(errs, err)
	}
	for name, rule := range c.WorkflowRules {
		if strings.TrimSpace(rule.RequestEvent) != "interactive" {
			errs = append(errs, fmt.Errorf("workflow-rules[%s].request_event must be interactive", name))
		}
		if len(rule.Match) == 0 {
			errs = append(errs, fmt.Errorf("workflow-rules[%s].match is required", name))
		}
		if len(rule.Triggers) == 0 {
			errs = append(errs, fmt.Errorf("workflow-rules[%s].trigger must contain at least one action", name))
		}
		for i, trigger := range rule.Triggers {
			if err := validateTrigger(trigger); err != nil {
				errs = append(errs, fmt.Errorf("workflow-rules[%s].trigger[%d]: %w", name, i, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (c ACPConfig) Validate() error {
	var errs []error
	if c.Enabled && strings.TrimSpace(c.Command) == "" {
		errs = append(errs, errors.New("acp.command is required when acp.enabled is true"))
	}
	for field, value := range map[string]string{
		"startup_timeout":        c.StartupTimeout,
		"request_timeout":        c.RequestTimeout,
		"session_idle_timeout":   c.SessionIdleTimeout,
		"stream_append_interval": c.StreamAppendInterval,
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, err := time.ParseDuration(value); err != nil {
			errs = append(errs, fmt.Errorf("acp.%s must be a valid duration: %w", field, err))
		}
	}
	if c.MaxSessions < 0 {
		errs = append(errs, errors.New("acp.max_sessions must be greater than or equal to zero"))
	}
	if c.StreamMinChunkChars < 0 {
		errs = append(errs, errors.New("acp.stream_min_chunk_chars must be greater than or equal to zero"))
	}
	return errors.Join(errs...)
}

func (c ACPConfig) EffectiveStartupTimeout() time.Duration {
	return durationOrDefault(c.StartupTimeout, 10*time.Second)
}

func (c ACPConfig) EffectiveRequestTimeout() time.Duration {
	return durationOrDefault(c.RequestTimeout, 10*time.Minute)
}

func (c ACPConfig) EffectiveSessionIdleTimeout() time.Duration {
	return durationOrDefault(c.SessionIdleTimeout, 30*time.Minute)
}

func (c ACPConfig) EffectiveStreamAppendInterval() time.Duration {
	return durationOrDefault(c.StreamAppendInterval, 250*time.Millisecond)
}

func (c ACPConfig) EffectiveMaxSessions() int {
	if c.MaxSessions > 0 {
		return c.MaxSessions
	}
	return 100
}

func (c ACPConfig) EffectiveStreamMinChunkChars() int {
	if c.StreamMinChunkChars > 0 {
		return c.StreamMinChunkChars
	}
	return 24
}

func durationOrDefault(value string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}

func validateTrigger(trigger TriggerConfig) error {
	switch trigger.Type {
	case "reply-to-slack":
		if trigger.ReplyToSlack == nil {
			return errors.New("reply-to-slack config is required")
		}
		hasTemplate := strings.TrimSpace(trigger.ReplyToSlack.Template) != ""
		hasRun := trigger.ReplyToSlack.Run != nil
		if hasTemplate == hasRun {
			return errors.New("reply-to-slack requires exactly one of template or run")
		}
		if hasRun {
			return validateRun(*trigger.ReplyToSlack.Run)
		}
	case "run":
		if trigger.Run == nil {
			return errors.New("run config is required")
		}
		return validateRun(*trigger.Run)
	default:
		return fmt.Errorf("unsupported trigger action %q", trigger.Type)
	}
	return nil
}

func validateRun(run RunTriggerConfig) error {
	if strings.TrimSpace(run.Cmd) == "" {
		return errors.New("cmd is required")
	}
	return nil
}
