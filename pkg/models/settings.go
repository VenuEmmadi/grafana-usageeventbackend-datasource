// pkg/models/settings.go
package models

import (
	"encoding/json"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

type PluginSettings struct {
	GrafanaURL string                `json:"grafanaURL"`
	Path       string                `json:"path"`
	Secrets    *SecretPluginSettings `json:"-"`
}

type SecretPluginSettings struct {
	ApiKey string `json:"apiKey"`
}

func LoadPluginSettings(source backend.DataSourceInstanceSettings) (*PluginSettings, error) {
	var settings PluginSettings
	if err := json.Unmarshal(source.JSONData, &settings); err != nil {
		return nil, fmt.Errorf("could not unmarshal PluginSettings json: %w", err)
	}
	settings.Secrets = loadSecretPluginSettings(source.DecryptedSecureJSONData)
	if settings.GrafanaURL == "" {
		settings.GrafanaURL = "http://localhost:3000"
	}
	return &settings, nil
}

func loadSecretPluginSettings(src map[string]string) *SecretPluginSettings {
	return &SecretPluginSettings{
		ApiKey: src["apiKey"],
	}
}
