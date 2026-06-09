package launchd

import (
	"bytes"
	"fmt"
	"text/template"
)

// plistTemplate matches the layout install.sh emitted, including the PATH
// environment variable so launchd-launched murtaugh resolves the same
// dependencies an interactive shell does.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>dev.murtaugh</string>
    <key>ProgramArguments</key>
    <array>
      <string>{{.Binary}}</string>
      <string>slack</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>WorkingDirectory</key>
    <string>{{.Home}}</string>
    <key>StandardOutPath</key>
    <string>{{.LogsDir}}/slack.out.log</string>
    <key>StandardErrorPath</key>
    <string>{{.LogsDir}}/slack.err.log</string>
    <key>EnvironmentVariables</key>
    <dict>
      <key>PATH</key>
      <string>{{.Home}}/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
  </dict>
</plist>
`

// renderPlist substitutes Binary, Home and LogsDir into plistTemplate.
func renderPlist(binary, home, logsDir string) ([]byte, error) {
	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse plist template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct {
		Binary  string
		Home    string
		LogsDir string
	}{Binary: binary, Home: home, LogsDir: logsDir}); err != nil {
		return nil, fmt.Errorf("render plist: %w", err)
	}
	return buf.Bytes(), nil
}
