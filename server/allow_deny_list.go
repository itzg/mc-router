package server

import (
	"encoding/json"
	"github.com/google/uuid"
	"os"
)

type AllowDenyLists struct {
	Allowlist []PlayerInfo
	Denylist []PlayerInfo
}

type AllowDenyConfig struct {
	Global AllowDenyLists
	Servers map[string]AllowDenyLists
}

func ParseAllowDenyConfig(allowDenyListPath string) (*AllowDenyConfig, error) {
	allowDenyConfig := AllowDenyConfig{}
	data, err := os.ReadFile(allowDenyListPath)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(data, &allowDenyConfig)
	if err != nil {
		return nil, err
	}
	return &allowDenyConfig, nil
}

func entryMatchesPlayer(entry *PlayerInfo, userInfo *PlayerInfo) bool {
	// User has added an "empty" entry
	// This should never match player info
	if entry.Name == "" && entry.Uuid == uuid.Nil {
		return false
	}
	
	if entry.Name != "" && entry.Uuid != uuid.Nil {
		return *entry == *userInfo
	}

	if entry.Uuid != uuid.Nil {
		return entry.Uuid == userInfo.Uuid
	}

	return entry.Name == userInfo.Name
}

func (allowDenyConfig *AllowDenyConfig) ServerAllowsPlayer(serverAddress string, userInfo *PlayerInfo) bool {
	if allowDenyConfig == nil {
		return true
	}

	allowlist := allowDenyConfig.Global.Allowlist
	denylist := allowDenyConfig.Global.Denylist
	serverAllowDenyConfig, ok := allowDenyConfig.Servers[serverAddress]
	if ok {
		allowlist = append(allowlist, serverAllowDenyConfig.Allowlist...)
		denylist = append(denylist, serverAllowDenyConfig.Denylist...)
	}

	for _, allowedPlayer := range allowlist {
		if entryMatchesPlayer(&allowedPlayer, userInfo) {
			return true
		}
	}

	if len(allowlist) > 0 {
		return false
	}

	for _, deniedPlayer := range denylist {
		if entryMatchesPlayer(&deniedPlayer, userInfo) {
			return false
		}
	}

	return true
}
