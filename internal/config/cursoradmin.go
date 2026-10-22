package config

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// UnmarshalYAML closes YAML's float-to-int coercion for day counts and retains
// the loader's unknown-field rule inside this custom decoder.
func (c *CursorAdminConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("collectors.cursor_admin must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		expected := "!!str"
		switch key {
		case "api_key", "api_base_url", "poll_interval":
		case "settle_days", "lookback_days":
			expected = "!!int"
		default:
			return fmt.Errorf("unknown collectors.cursor_admin field %q", key)
		}
		if value.Kind != yaml.ScalarNode || value.Tag != expected {
			return fmt.Errorf("collectors.cursor_admin.%s has the wrong scalar type", key)
		}
	}
	type plain CursorAdminConfig
	if err := node.Decode((*plain)(c)); err != nil {
		return fmt.Errorf("invalid collectors.cursor_admin mapping")
	}
	return nil
}
