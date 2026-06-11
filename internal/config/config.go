package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultRelativePath = ".config/murtaugh/slack.yaml"
const defaultAgentsRelativePath = ".config/murtaugh/agents.yaml"
const defaultJobsRelativePath = ".config/murtaugh/jobs.yaml"

type Config struct {
	BaseDir       string                        `yaml:"-"`
	OAuth         OAuthConfig                   `yaml:"oauth"`
	Configuration ConfigurationConfig           `yaml:"configuration"`
	Chat          ChatConfig                    `yaml:"chat"`
	ACP           ACPConfig                     `yaml:"-"`
	Agents        map[string]AgentProfile       `yaml:"-"`
	Jobs          map[string]JobProfile         `yaml:"-"`
	Commands      []CommandConfig               `yaml:"commands"`
	WorkflowRules map[string]WorkflowRuleConfig `yaml:"workflow-rules"`
	UnfurlRules   map[string]UnfurlRuleConfig   `yaml:"unfurl-rules"`
}

type OAuthConfig struct {
	AppToken string `yaml:"app_token"`
	BotToken string `yaml:"bot_token"`
}

type ConfigurationConfig struct {
	AdminUser    string   `yaml:"admin_user"`
	AllowedUsers []string `yaml:"allowed_users"`
	Debug        bool     `yaml:"debug"`
}

type ChatConfig struct {
	ChannelAgents map[string]string `yaml:"channel_agents"`
	DMAgent       string            `yaml:"dm_agent"`
	DefaultAgent  string            `yaml:"default_agent"`
}

type CommandConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type ACPConfig struct {
	Enabled              bool   `yaml:"enabled"`
	StartupTimeout       string `yaml:"startup_timeout"`
	RequestTimeout       string `yaml:"request_timeout"`
	SessionIdleTimeout   string `yaml:"session_idle_timeout"`
	MaxSessions          int    `yaml:"max_sessions"`
	StreamAppendInterval string `yaml:"stream_append_interval"`
	StreamMinChunkChars  int    `yaml:"stream_min_chunk_chars"`
	StreamFinalFeedback  bool   `yaml:"stream_final_feedback"`
	CancelGracePeriod    string `yaml:"cancel_grace_period"`
}

type AgentProfile struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	WorkDir string   `yaml:"workdir"`
	// Interruptible overrides auto-detection of session/cancel support. When
	// nil (the default) Murtaugh probes the agent at warmup; set it explicitly
	// to skip the probe or to correct a wrong verdict.
	Interruptible *bool `yaml:"interruptible"`
}

type JobProfile struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	WorkDir string   `yaml:"workdir"`
	Timeout string   `yaml:"timeout"`
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

type UnfurlRuleConfig struct {
	Match  UnfurlMatchConfig  `yaml:"match"`
	Unfurl UnfurlActionConfig `yaml:"unfurl"`
}

type UnfurlMatchConfig struct {
	Channels   []string `yaml:"channels"`
	Domain     string   `yaml:"domain"`
	URLPrefix  string   `yaml:"url_prefix"`
	URLPattern string   `yaml:"url_pattern"`
}

type UnfurlActionConfig struct {
	Template string            `yaml:"template"`
	Run      *RunTriggerConfig `yaml:"run"`
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

func DefaultAgentsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, defaultAgentsRelativePath), nil
}

func DefaultJobsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, defaultJobsRelativePath), nil
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

	agentsPath := filepath.Join(cfg.BaseDir, "agents.yaml")
	agentsData, err := os.ReadFile(agentsPath)
	if err == nil {
		var agents struct {
			ACP    ACPConfig               `yaml:"acp"`
			Agents map[string]AgentProfile `yaml:"agents"`
		}
		if err := yaml.Unmarshal(agentsData, &agents); err != nil {
			return Config{}, fmt.Errorf("parse agents config %q: %w", agentsPath, err)
		}
		cfg.ACP = agents.ACP
		cfg.Agents = agents.Agents
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read agents config %q: %w", agentsPath, err)
	}

	jobsPath := filepath.Join(cfg.BaseDir, "jobs.yaml")
	jobsData, err := os.ReadFile(jobsPath)
	if err == nil {
		var jobs struct {
			Jobs map[string]JobProfile `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(jobsData, &jobs); err != nil {
			return Config{}, fmt.Errorf("parse jobs config %q: %w", jobsPath, err)
		}
		cfg.Jobs = jobs.Jobs
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read jobs config %q: %w", jobsPath, err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.OAuth.AppToken) == "" {
		errs = append(errs, errors.New("oauth.app_token is required"))
	}
	if strings.TrimSpace(c.OAuth.BotToken) == "" {
		errs = append(errs, errors.New("oauth.bot_token is required"))
	}
	for i, command := range c.Commands {
		if !strings.HasPrefix(strings.TrimSpace(command.Name), "/") {
			errs = append(errs, fmt.Errorf("commands[%d].name must start with /", i))
		}
	}
	for i, allowed := range c.Configuration.AllowedUsers {
		if strings.TrimSpace(allowed) == "" {
			errs = append(errs, fmt.Errorf("configuration.allowed_users[%d] must not be blank", i))
		}
	}
	if err := c.ACP.Validate(); err != nil {
		errs = append(errs, err)
	}

	if c.ACP.Enabled {
		if len(c.Agents) == 0 {
			errs = append(errs, errors.New("acp is enabled but no agents are defined in agents.yaml"))
		}
		if strings.TrimSpace(c.Chat.DefaultAgent) == "" {
			errs = append(errs, errors.New("chat.default_agent is required when acp is enabled"))
		} else if _, ok := c.Agents[c.Chat.DefaultAgent]; !ok {
			errs = append(errs, fmt.Errorf("chat.default_agent %q not found in agents.yaml", c.Chat.DefaultAgent))
		}
		if c.Chat.DMAgent != "" {
			if _, ok := c.Agents[c.Chat.DMAgent]; !ok {
				errs = append(errs, fmt.Errorf("chat.dm_agent %q not found in agents.yaml", c.Chat.DMAgent))
			}
		}
		for channel, agent := range c.Chat.ChannelAgents {
			if _, ok := c.Agents[agent]; !ok {
				errs = append(errs, fmt.Errorf("chat.channel_agents[%s] references unknown agent %q", channel, agent))
			}
		}
	}

	for name, job := range c.Jobs {
		if strings.TrimSpace(job.Command) == "" {
			errs = append(errs, fmt.Errorf("jobs[%s].command is required", name))
		}
		if job.Timeout != "" {
			if _, err := time.ParseDuration(job.Timeout); err != nil {
				errs = append(errs, fmt.Errorf("jobs[%s].timeout must be a valid duration: %w", name, err))
			}
		}
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
	for name, rule := range c.UnfurlRules {
		if err := validateUnfurlRule(rule); err != nil {
			errs = append(errs, fmt.Errorf("unfurl-rules[%s]: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// IsAllowedUser reports whether the given Slack user ID is permitted to
// interact directly with the bot via slash commands, mentions, or DMs.
//
// The check is ID-only: a user is allowed when their ID matches
// configuration.admin_user (only when admin_user is configured as a Slack
// user ID, not a handle) or any entry in configuration.allowed_users. The
// slackapp startup layer is responsible for resolving any handle entries in
// allowed_users to IDs before this helper is consulted, and for separately
// checking against the resolved admin user ID when admin_user is a handle.
//
// With a tight default, an empty allowed_users list means only the admin user
// may interact with the bot.
func (c ConfigurationConfig) IsAllowedUser(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	admin := strings.TrimPrefix(strings.TrimSpace(c.AdminUser), "@")
	if looksLikeSlackUserID(admin) && admin == userID {
		return true
	}
	for _, allowed := range c.AllowedUsers {
		if strings.TrimSpace(allowed) == userID {
			return true
		}
	}
	return false
}

// IsAdminUser reports whether the given Slack user ID matches the
// resolved configuration.admin_user. Like IsAllowedUser this is ID-only,
// so admin_user must already have been resolved from a handle to a Slack
// user ID (slackapp.resolveAllowSet does this at daemon start). A blank
// or handle-shaped admin_user will never match.
func (c ConfigurationConfig) IsAdminUser(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	admin := strings.TrimPrefix(strings.TrimSpace(c.AdminUser), "@")
	return looksLikeSlackUserID(admin) && admin == userID
}

func looksLikeSlackUserID(value string) bool {
	if len(value) < 4 {
		return false
	}
	if !(strings.HasPrefix(value, "U") || strings.HasPrefix(value, "W")) {
		return false
	}
	for _, r := range value[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

func (c ACPConfig) Validate() error {
	var errs []error
	for field, value := range map[string]string{
		"startup_timeout":        c.StartupTimeout,
		"request_timeout":        c.RequestTimeout,
		"session_idle_timeout":   c.SessionIdleTimeout,
		"stream_append_interval": c.StreamAppendInterval,
		"cancel_grace_period":    c.CancelGracePeriod,
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

func (p AgentProfile) Validate() error {
	if strings.TrimSpace(p.Command) == "" {
		return errors.New("agent profile command is required")
	}
	return nil
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

func (c ACPConfig) EffectiveCancelGracePeriod() time.Duration {
	return durationOrDefault(c.CancelGracePeriod, 2*time.Second)
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

func validateUnfurlRule(rule UnfurlRuleConfig) error {
	var errs []error
	match := rule.Match
	if strings.TrimSpace(match.Domain) == "" &&
		strings.TrimSpace(match.URLPrefix) == "" &&
		strings.TrimSpace(match.URLPattern) == "" {
		errs = append(errs, errors.New("match requires at least one of domain, url_prefix, url_pattern"))
	}
	if pattern := strings.TrimSpace(match.URLPattern); pattern != "" {
		if _, err := regexp.Compile(pattern); err != nil {
			errs = append(errs, fmt.Errorf("match.url_pattern is not a valid regexp: %w", err))
		}
	}
	for i, channel := range match.Channels {
		if strings.TrimSpace(channel) == "" {
			errs = append(errs, fmt.Errorf("match.channels[%d] must not be blank", i))
		}
	}
	hasTemplate := strings.TrimSpace(rule.Unfurl.Template) != ""
	hasRun := rule.Unfurl.Run != nil
	if hasTemplate == hasRun {
		errs = append(errs, errors.New("unfurl requires exactly one of template or run"))
	}
	if hasRun {
		if err := validateRun(*rule.Unfurl.Run); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
