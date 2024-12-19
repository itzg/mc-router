package server

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"net/netip"
	"testing"
)

func TestClientFilter_Allow(t *testing.T) {
	type args struct {
		allow []string
		deny  []string
		input string
	}
	tests := []struct {
		name      string
		args      args
		want      bool
		assertErr assert.ErrorAssertionFunc
	}{
		{
			name: "defaults",
			args: args{
				input: "192.168.1.1",
			},
			want:      true,
			assertErr: assert.NoError,
		},
		{
			name: "just allow - matches",
			args: args{
				allow: []string{"192.168.1.1"},
				input: "192.168.1.1",
			},
			want:      true,
			assertErr: assert.NoError,
		},
		{
			name: "just allow - not match",
			args: args{
				allow: []string{"192.168.1.1"},
				input: "192.168.1.2",
			},
			want:      false,
			assertErr: assert.NoError,
		},
		{
			name: "just allow cidr - matches",
			args: args{
				allow: []string{"192.168.1.0/8"},
				input: "192.168.1.2",
			},
			want:      true,
			assertErr: assert.NoError,
		},
		{
			name: "just allow cidr or specific - matches cidr",
			args: args{
				allow: []string{"192.168.1.0/8", "192.168.2.5"},
				input: "192.168.1.2",
			},
			want:      true,
			assertErr: assert.NoError,
		},
		{
			name: "just allow cidr or specific - matches specific",
			args: args{
				allow: []string{"192.168.1.0/8", "192.168.2.5"},
				input: "192.168.2.5",
			},
			want:      true,
			assertErr: assert.NoError,
		},
		{
			name: "just deny - matches",
			args: args{
				deny:  []string{"192.168.2.5"},
				input: "192.168.2.5",
			},
			want:      false,
			assertErr: assert.NoError,
		},
		{
			name: "just deny - not match",
			args: args{
				deny:  []string{"192.168.2.5"},
				input: "192.168.1.1",
			},
			want:      true,
			assertErr: assert.NoError,
		},
		{
			name: "mix allow",
			args: args{
				allow: []string{"192.168.1.6"},
				deny:  []string{"192.168.1.0/8"},
				input: "192.168.1.6",
			},
			want:      true,
			assertErr: assert.NoError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := NewClientFilter(tt.args.allow, tt.args.deny)
			assert.NoError(t, err)
			addr, err := netip.ParseAddr(tt.args.input)
			assert.NoError(t, err)
			got := f.Allow(netip.AddrPortFrom(addr, 25565))
			assert.Equalf(t, tt.want, got, "Allow(%v)", tt.args.input)
		})
	}
}

func TestNewClientFilter(t *testing.T) {
	type args struct {
		allow []string
		deny  []string
	}
	tests := []struct {
		name      string
		args      args
		assertErr assert.ErrorAssertionFunc
	}{
		{
			name:      "default",
			assertErr: assert.NoError,
		},
		{
			name: "allow single",
			args: args{
				allow: []string{"192.168.1.1"},
			},
			assertErr: assert.NoError,
		},
		{
			name: "allow cidr",
			args: args{
				allow: []string{"192.168.1.0/8"},
			},
			assertErr: assert.NoError,
		},
		{
			name: "deny single",
			args: args{
				deny: []string{"192.168.1.1"},
			},
			assertErr: assert.NoError,
		},
		{
			name: "allow invalid",
			args: args{
				allow: []string{"7"},
			},
			assertErr: assert.Error,
		},
		{
			name: "deny invalid",
			args: args{
				deny: []string{"7"},
			},
			assertErr: assert.Error,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClientFilter(tt.args.allow, tt.args.deny)
			tt.assertErr(t, err, fmt.Sprintf("NewClientFilter(%v, %v)", tt.args.allow, tt.args.deny))
		})
	}
}
