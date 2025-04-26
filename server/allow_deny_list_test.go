package server

import (
	"github.com/google/uuid"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_allowDenyConfig_ServerAllowsPlayer(t *testing.T) {
	type args struct {
		serverAddress string
		userInfo *PlayerInfo
	}
	validUserInfo := &PlayerInfo{
		Name: "player_name",
		Uuid: uuid.MustParse("53036a8f-cbc8-4074-bbc5-98e5e19b0b14"),
	}
	otherUserInfo := &PlayerInfo{
		Name: "other_player",
		Uuid: uuid.MustParse("0d51a0ca-f498-44bf-813f-635c18594b8c"),
	}
	tests := []struct {
		name            string
		allowDenyConfig *AllowDenyConfig
		args            args
		want            bool
	}{
		{
			name: "nil config",
			allowDenyConfig: nil,
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: true,
		},
		{
			name: "empty config",
			allowDenyConfig: &AllowDenyConfig{},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: true,
		},
		{
			name: "impossible global allowlist",
			allowDenyConfig: &AllowDenyConfig{
				Global: AllowDenyLists{
					Allowlist: []PlayerInfo{
						PlayerInfo{
							Name: "",
							Uuid: uuid.Nil,
						},
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: false,
		},
		{
			name: "player allowed globally",
			allowDenyConfig: &AllowDenyConfig{
				Global: AllowDenyLists{
					Allowlist: []PlayerInfo{
						*validUserInfo,
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: true,
		},
		{
			name: "player not in allowlist",
			allowDenyConfig: &AllowDenyConfig{
				Global: AllowDenyLists{
					Allowlist: []PlayerInfo{
						*otherUserInfo,
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: false,
		},
		{
			name: "player denied globally",
			allowDenyConfig: &AllowDenyConfig{
				Global: AllowDenyLists{
					Denylist: []PlayerInfo{
						*validUserInfo,
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: false,
		},
		{
			name: "player allowed and denied globally",
			allowDenyConfig: &AllowDenyConfig{
				Global: AllowDenyLists{
					Allowlist: []PlayerInfo{
						*validUserInfo,
					},
					Denylist: []PlayerInfo{
						*validUserInfo,
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: true,
		},
		{
			name: "player allowed on server",
			allowDenyConfig: &AllowDenyConfig{
				Servers: map[string]AllowDenyLists{
					"server.my.domain": AllowDenyLists{
						Allowlist: []PlayerInfo{
							*validUserInfo,
						},
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: true,
		},
		{
			name: "player not allowed on server",
			allowDenyConfig: &AllowDenyConfig{
				Servers: map[string]AllowDenyLists{
					"server.my.domain": AllowDenyLists{
						Allowlist: []PlayerInfo{
							*otherUserInfo,
						},
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: false,
		},
		{
			name: "player denied on server",
			allowDenyConfig: &AllowDenyConfig{
				Servers: map[string]AllowDenyLists{
					"server.my.domain": AllowDenyLists{
						Denylist: []PlayerInfo{
							*validUserInfo,
						},
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: false,
		},
		{
			name: "player allowed globally but denied on server",
			allowDenyConfig: &AllowDenyConfig{
				Global: AllowDenyLists{
					Allowlist: []PlayerInfo{
						*validUserInfo,
					},
				},
				Servers: map[string]AllowDenyLists{
					"server.my.domain": AllowDenyLists{
						Denylist: []PlayerInfo{
							*validUserInfo,
						},
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: true,
		},
		{
			name: "player denied globally but allowed on server",
			allowDenyConfig: &AllowDenyConfig{
				Global: AllowDenyLists{
					Denylist: []PlayerInfo{
						*validUserInfo,
					},
				},
				Servers: map[string]AllowDenyLists{
					"server.my.domain": AllowDenyLists{
						Allowlist: []PlayerInfo{
							*validUserInfo,
						},
					},
				},
			},
			args: args{
				serverAddress: "server.my.domain",
				userInfo: validUserInfo,
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := tt.allowDenyConfig.ServerAllowsPlayer(tt.args.serverAddress, tt.args.userInfo)
			assert.Equal(t, tt.want, allowed)
		})
	}
}
