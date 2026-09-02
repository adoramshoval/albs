package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadRepoMap reads a custom mapping of OCI Image URIs to Git URLs from JSON or YAML files.
func LoadRepoMap(filePath string) (map[string]string, error) {
	if filePath == "" {
		return make(map[string]string), nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read repo map file: %w", err)
	}

	repoMap := make(map[string]string)
	ext := strings.ToLower(filepath.Ext(filePath))

	switch ext {
	case ".json":
		if err := json.Unmarshal(data, &repoMap); err != nil {
			return nil, fmt.Errorf("failed to parse JSON repo map: %w", err)
		}
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &repoMap); err != nil {
			return nil, fmt.Errorf("failed to parse YAML repo map: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported repo map format %q (expected .json, .yaml, or .yml)", ext)
	}

	return repoMap, nil
}
