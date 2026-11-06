package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// RouteConfig accepts a backend string or an object with per-route settings.
// Both forms can appear in mappings or default-server:
//
//	"backend:25565"
//	{"backend": "backend:25565", "proxy-protocol": false}
type RouteConfig struct {
	Backend       string `json:"backend"`
	ProxyProtocol *bool  `json:"proxy-protocol,omitempty"`
}

// UnmarshalJSON accepts both the original backend string and the object form.
// An omitted proxy-protocol setting inherits the router's global setting.
func (r *RouteConfig) UnmarshalJSON(data []byte) error {
	var backend string
	if err := json.Unmarshal(data, &backend); err == nil {
		*r = RouteConfig{Backend: backend}
		return nil
	}

	// Fall back to unmarshaling as a full object ({"backend": "...", "proxy-protocol": false}).
	type alias RouteConfig // Prevent infinite recursion through UnmarshalJSON.
	var config alias
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return err
	}
	if strings.TrimSpace(config.Backend) == "" {
		return fmt.Errorf("route object requires a nonempty backend")
	}
	*r = RouteConfig(config)
	return nil
}

// MarshalJSON keeps routes without overrides in the original string form.
func (r RouteConfig) MarshalJSON() ([]byte, error) {
	if r.ProxyProtocol == nil {
		return json.Marshal(r.Backend)
	}
	type alias RouteConfig
	return json.Marshal(alias(r))
}
