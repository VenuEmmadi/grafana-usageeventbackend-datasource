// pkg/models/settings.go
package models

import (
	"encoding/json"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

type PluginSettings struct {
	GrafanaURL string                `json:"grafanaURL"`
	Secrets    *SecretPluginSettings `json:"-"`
	// You can add DB config fields here if you wish to allow connection override.
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
		settings.GrafanaURL = "http://localhost:3001"
	}
	return &settings, nil
}

func loadSecretPluginSettings(src map[string]string) *SecretPluginSettings {
	return &SecretPluginSettings{
		ApiKey: src["apiKey"],
	}
}
