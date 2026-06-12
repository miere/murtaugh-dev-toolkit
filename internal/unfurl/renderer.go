package unfurl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"text/template"

	"github.com/miere/murtaugh-dev-toolkit/assets"
	"github.com/slack-go/slack"
)

// Data is the context exposed to unfurl templates and passed as JSON on stdin
// to `run` handlers. Field names are intentionally exported and stable.
type Data struct {
	URL       string
	Domain    string
	Channel   string
	User      string
	MessageTS string
	ThreadTS  string
	TeamID    string
	Captures  map[string]string
}

// Renderer turns a Block Kit JSON template into a slack.Attachment. It loads
// templates from the configured directory first, then the embedded assets FS,
// matching the workflow engine's resolution order.
type Renderer struct {
	templateDir string
	templateFS  fs.FS
}

// NewRenderer builds a Renderer. A nil templateFS falls back to assets.FS.
func NewRenderer(templateDir string, templateFS fs.FS) *Renderer {
	if templateFS == nil {
		templateFS = assets.FS
	}
	if templateDir == "" {
		templateDir = "."
	}
	return &Renderer{templateDir: templateDir, templateFS: templateFS}
}

// Render parses and executes the template at path with data, returning the
// decoded attachment.
func (r *Renderer) Render(path string, data Data) (slack.Attachment, error) {
	resolved := r.resolve(path)
	content, err := r.read(path, resolved)
	if err != nil {
		return slack.Attachment{}, fmt.Errorf("read template: %w", err)
	}
	tpl, err := template.New(filepath.Base(resolved)).Option("missingkey=error").Parse(string(content))
	if err != nil {
		return slack.Attachment{}, fmt.Errorf("parse template: %w", err)
	}
	var rendered bytes.Buffer
	if err := tpl.Execute(&rendered, data); err != nil {
		return slack.Attachment{}, fmt.Errorf("execute template: %w", err)
	}
	return ParseAttachment(rendered.Bytes())
}

// RenderPrompt renders a delegate-to-agent prompt template against the unfurl
// Data (so prompts can reference {{ .URL }}, {{ .Captures.number }}, etc.),
// using missingkey=error so a typo'd placeholder fails loudly rather than
// sending the agent a half-rendered prompt.
func RenderPrompt(promptTemplate string, data Data) (string, error) {
	tpl, err := template.New("prompt").Option("missingkey=error").Parse(promptTemplate)
	if err != nil {
		return "", fmt.Errorf("parse prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute prompt template: %w", err)
	}
	return buf.String(), nil
}

// ParseAttachment decodes a single Slack attachment (which may carry Block Kit
// blocks) from JSON, rejecting malformed output.
func ParseAttachment(body []byte) (slack.Attachment, error) {
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		return slack.Attachment{}, fmt.Errorf("unfurl output must be valid JSON")
	}
	var attachment slack.Attachment
	if err := json.Unmarshal(trimmed, &attachment); err != nil {
		return slack.Attachment{}, fmt.Errorf("decode unfurl attachment: %w", err)
	}
	return attachment, nil
}

func (r *Renderer) resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(r.templateDir, path)
}

func (r *Renderer) read(templatePath, resolvedPath string) ([]byte, error) {
	content, err := os.ReadFile(resolvedPath)
	if err == nil {
		return content, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	if r.templateFS != nil && !filepath.IsAbs(templatePath) {
		return fs.ReadFile(r.templateFS, filepath.ToSlash(templatePath))
	}
	return nil, err
}
