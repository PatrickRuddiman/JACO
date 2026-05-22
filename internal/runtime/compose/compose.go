package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
)

// Load reads a compose file at path and returns the parsed Project plus the
// raw bytes (so Validate can do a strict closed-field-set check without
// re-reading the file from disk).
func Load(path string) (*types.Project, []byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	project, err := loadBytes(filepath.Dir(path), filepath.Base(path), body)
	if err != nil {
		return nil, nil, err
	}
	return project, body, nil
}

// LoadBytes parses a compose document supplied as raw bytes (the in-flight
// Deploy.Apply path receives YAML over gRPC — no file on disk).
func LoadBytes(body []byte, virtualFilename string) (*types.Project, error) {
	return loadBytes(".", virtualFilename, body)
}

func loadBytes(workingDir, filename string, body []byte) (*types.Project, error) {
	details := types.ConfigDetails{
		WorkingDir: workingDir,
		ConfigFiles: []types.ConfigFile{{
			Filename: filename,
			Content:  body,
		}},
		Environment: map[string]string{},
	}
	project, err := loader.LoadWithContext(context.Background(), details, func(opts *loader.Options) {
		opts.SetProjectName("jaco", true)
		// We do our own field-level validation; let compose-go normalize but
		// skip its consistency check which can reject otherwise-valid graphs.
		opts.SkipConsistencyCheck = true
	})
	if err != nil {
		return nil, fmt.Errorf("compose load: %w", err)
	}
	return project, nil
}
