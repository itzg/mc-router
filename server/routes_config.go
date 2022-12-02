package server

import (
	"encoding/json"
	"github.com/pkg/errors"
	"io/fs"
	"os"
)


func ReadRoutesConfig(routesConfig string) error {
	file, fileErr := os.ReadFile(routesConfig)
	if fileErr != nil {
		if errors.Is(fileErr, fs.ErrNotExist) {
			// File doesn't exist -> ignore it
			return nil
		}
		return errors.Wrap(fileErr, "Could not load the routes config file")
	}

	configMappings := map[string]string{}

	parseErr := json.Unmarshal(file, &configMappings)
	if parseErr != nil {
		return errors.Wrap(parseErr, "Could not parse the json routes config file")
	}

	Routes.RegisterAll(configMappings)
	return nil
}
