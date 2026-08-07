package server

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func Test_apiServer_routesListHandler(t *testing.T) {
	tests := []struct {
		name          string
		scalingTarget ScalingTarget
		want          string
	}{
		{name: "with scaling target", scalingTarget: &testingScalingTarget{name: "sc1"}, want: "{\"mc.example.com\":{\"backend\": \"backend:25565\", \"scalingTarget\": \"sc1\"}}"},
		{name: "nil scaling target", scalingTarget: nil, want: "{\"mc.example.com\":{\"backend\": \"backend:25565\", \"scalingTarget\": \"\"}}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := new(MockRoutes)
			mappings := map[string]string{}
			mappings["mc.example.com"] = "backend:25565"

			routes.On("GetMappings").Return(mappings)
			routes.On("FindBackendForServerAddress", mock.Anything, "mc.example.com").Return("backend:25565", "mc.example.com",
				tt.scalingTarget, nillableWaker(nil), nillableSleeper(nil))
			server, err := StartApiServer(t.Context(), "127.0.0.1:0", routes, nil, nil)
			if err != nil {
				require.NotNil(t, err)
			}

			serverAddr := server.GetAddr()
			// get listing by http request to serverAddr

			//goland:noinspection HttpUrlsUsage
			resp, err := http.Get("http://" + serverAddr + "/routes")
			require.NoError(t, err)
			//goland:noinspection GoUnhandledErrorResult
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(body))
		})
	}

}
