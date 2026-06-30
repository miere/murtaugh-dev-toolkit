package migrate

import (
	"path/filepath"
)

// detectV2 reports whether gateway.yaml's chat block still carries the pre-v2
// flat shape: any of `default_agent`, `dm_agent`, or `channel_agents` directly
// under `chat`. These were superseded by `chat.defaults.{agent,dm_agent}` and
// `chat.channels.<k>.agent`.
func detectV2(dir string) bool {
	root := readYAML(filepath.Join(dir, "gateway.yaml"))
	if root == nil {
		return false
	}
	chat, ok := asMap(root["chat"])
	if !ok {
		return false
	}
	for _, k := range []string{"default_agent", "dm_agent", "channel_agents"} {
		if _, has := chat[k]; has {
			return true
		}
	}
	return false
}

// applyV2 rewrites the legacy flat chat block into the nested shape:
//   - chat.default_agent      → chat.defaults.agent
//   - chat.dm_agent           → chat.defaults.dm_agent
//   - chat.channel_agents.<k> → chat.channels.<k>.agent
//
// no_mention and enabled are left untouched. The transform is idempotent: once
// the three legacy keys are gone it is a no-op (detectV2 returns false).
func applyV2(dir string) error {
	gatewayPath := filepath.Join(dir, "gateway.yaml")
	root := readYAML(gatewayPath)
	if root == nil {
		return nil
	}
	chat, ok := asMap(root["chat"])
	if !ok {
		return nil
	}

	// Fold default_agent / dm_agent into chat.defaults, preserving any defaults
	// the user may have already written.
	defaults, _ := asMap(chat["defaults"])
	if defaults == nil {
		defaults = map[string]any{}
	}
	moveKey(defaults, chat, "default_agent", "agent")
	moveKey(defaults, chat, "dm_agent", "dm_agent")
	if len(defaults) > 0 {
		chat["defaults"] = defaults
	}

	// channel_agents (name → agent) → channels (name → {agent: name}).
	if ca, ok := asMap(chat["channel_agents"]); ok {
		channels, _ := asMap(chat["channels"])
		if channels == nil {
			channels = map[string]any{}
		}
		for key, agent := range ca {
			entry, _ := asMap(channels[key])
			if entry == nil {
				entry = map[string]any{}
			}
			entry["agent"] = agent
			channels[key] = entry
		}
		if len(channels) > 0 {
			chat["channels"] = channels
		}
		delete(chat, "channel_agents")
	}

	root["chat"] = chat
	return writeYAML(gatewayPath, root)
}
