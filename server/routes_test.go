package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_routesImpl_FindBackendForServerAddress(t *testing.T) {
	type args struct {
		serverAddress string
	}
	type mapping struct {
		serverAddress string
		backend       string
	}
	tests := []struct {
		name    string
		mapping mapping
		args    args
		want    string
	}{
		{
			name: "typical",
			mapping: mapping{
				serverAddress: "typical.my.domain", backend: "backend:25565",
			},
			args: args{
				serverAddress: `typical.my.domain`,
			},
			want: "backend:25565",
		},
		{
			name: "forge",
			mapping: mapping{
				serverAddress: "forge.my.domain", backend: "backend:25566",
			},
			args: args{
				serverAddress: "forge.my.domain\x00FML2\x00",
			},
			want: "backend:25566",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRoutes()

			r.CreateMapping(tt.mapping.serverAddress, tt.mapping.backend, func(ctx context.Context) error { return nil })

			if got, server := r.FindBackendForServerAddress(context.Background(), tt.args.serverAddress); got != tt.want {
				t.Errorf("routesImpl.FindBackendForServerAddress() = %v, want %v", got, tt.want)
			} else {
				assert.Equal(t, tt.mapping.serverAddress, server)
			}
		})
	}
}
