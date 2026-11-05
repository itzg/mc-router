package server

import "encoding/json"

// RouteConfig accepts a backend string or an object with per-route settings.
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

	type alias RouteConfig
	var config alias
	if err := json.Unmarshal(data, &config); err != nil {
		return err
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
