package cluster

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

var guestUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)

func cloudConfig(input, username, publicKey string) (string, error) {
	if !guestUser.MatchString(username) || username == "default" {
		return "", fmt.Errorf("username must be a valid guest login name")
	}
	config := map[string]any{}
	if strings.TrimSpace(input) != "" {
		if !strings.HasPrefix(input, "#cloud-config") {
			return "", fmt.Errorf("user-data must start with #cloud-config")
		}
		converted, err := yaml.YAMLToJSONStrict([]byte(input))
		if err != nil {
			return "", fmt.Errorf("parse user-data: %w", err)
		}
		if err := json.Unmarshal(converted, &config); err != nil || config == nil {
			return "", fmt.Errorf("user-data must be a cloud-config mapping")
		}
	}
	if raw, ok := config["user"]; ok {
		if _, hasUsers := config["users"]; hasUsers {
			return "", fmt.Errorf("user-data must use either user or users, not both")
		}
		name, err := userName(raw)
		if err != nil || name != username {
			return "", fmt.Errorf("user-data defines a conflicting default user")
		}
	}
	if raw, ok := config["users"]; ok {
		users, ok := raw.([]any)
		if !ok {
			return "", fmt.Errorf("user-data users must be a list")
		}
		matches := 0
		for i, entry := range users {
			name, err := userName(entry)
			if err != nil {
				return "", fmt.Errorf("user-data users[%d]: %w", i, err)
			}
			if name != username {
				if strings.Contains(name, ",") {
					for _, part := range strings.Split(name, ",") {
						if strings.TrimSpace(part) == username {
							return "", fmt.Errorf("user-data defines username in a combined users entry")
						}
					}
				}
				continue
			}
			matches++
			if matches > 1 {
				return "", fmt.Errorf("user-data defines username more than once")
			}
			user := userMap(entry, username)
			if err := appendKey(user, publicKey); err != nil {
				return "", err
			}
			users[i] = user
		}
		if matches == 0 {
			users = append(users, map[string]any{"name": username, "ssh_authorized_keys": []any{publicKey}})
		}
		config["users"] = users
	} else {
		user := userMap(config["user"], username)
		if err := appendKey(user, publicKey); err != nil {
			return "", err
		}
		config["user"] = user
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("encode user-data: %w", err)
	}
	result := "#cloud-config\n" + string(encoded)
	if len(result) > 64*1024 {
		return "", fmt.Errorf("merged user-data exceeds 64 KiB")
	}
	return result, nil
}

func userName(entry any) (string, error) {
	switch value := entry.(type) {
	case string:
		if value == "" {
			return "", fmt.Errorf("empty user name")
		}
		return value, nil
	case map[string]any:
		name, ok := value["name"].(string)
		if !ok || name == "" {
			return "", fmt.Errorf("user entry needs a name")
		}
		return name, nil
	default:
		return "", fmt.Errorf("user entry must be a name or mapping")
	}
}

func userMap(entry any, username string) map[string]any {
	if value, ok := entry.(map[string]any); ok {
		return value
	}
	return map[string]any{"name": username}
}

func appendKey(user map[string]any, key string) error {
	raw, exists := user["ssh_authorized_keys"]
	if !exists {
		user["ssh_authorized_keys"] = []any{key}
		return nil
	}
	keys, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("ssh_authorized_keys must be a list")
	}
	for _, item := range keys {
		value, ok := item.(string)
		if !ok {
			return fmt.Errorf("ssh_authorized_keys must contain strings")
		}
		if value == key {
			return nil
		}
	}
	user["ssh_authorized_keys"] = append(keys, key)
	return nil
}
