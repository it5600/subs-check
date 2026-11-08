package app

import (
	"os"
	"path/filepath"

	"substore-ui-go/internal/subscheck/config"
	"substore-ui-go/internal/model"
	"substore-ui-go/internal/service"
	"substore-ui-go/internal/subscheck/utils"
)

// loadNodesLocal parses the results file written by the last check round.
// The merged binary writes output/all.yaml itself, so the panel does not need
// an HTTP round-trip to its own subscription endpoint just to list nodes.
func (a *SubStoreApp) loadNodesLocal() ([]model.Node, bool) {
	outDir := config.GlobalConfig.OutputDir
	if outDir == "" {
		base := utils.GetExecutablePath()
		if base == "" {
			return nil, false
		}
		outDir = filepath.Join(base, "output")
	}
	data, err := os.ReadFile(filepath.Join(outDir, "all.yaml"))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	nodes := service.ParseNodesFromYaml(string(data))
	if len(nodes) == 0 {
		return nil, false
	}
	return nodes, true
}