package rust

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/sst/sst/v3/internal/fs"
	"github.com/sst/sst/v3/pkg/process"
	"github.com/sst/sst/v3/pkg/runtime"
)

type Runtime struct {
	mut         sync.Mutex
	directories map[string]string
}

type Worker struct {
	stdout io.ReadCloser
	stderr io.ReadCloser
	cmd    *exec.Cmd
}

func (w *Worker) Stop() {
	process.Kill(w.cmd.Process)
}

func (w *Worker) Logs() io.ReadCloser {
	reader, writer := io.Pipe()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(writer, w.stdout)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(writer, w.stderr)
	}()

	go func() {
		wg.Wait()
		defer writer.Close()
	}()

	return reader
}

func New() *Runtime {
	return &Runtime{
		directories: map[string]string{},
	}
}

func (r *Runtime) Match(runtime string) bool {
	return runtime == "rust"
}

type CargoConfigBuild struct {
	TargetDir *string `toml:"target-dir,omitempty"`
}

type CargoConfig struct {
	Build CargoConfigBuild `toml:"build"`
}

type CargoTomlBin struct {
	Name             string   `toml:"name,omitempty"`
	RequiredFeatures []string `toml:"required-features,omitempty"`
}

type CargoToml struct {
	Bin []CargoTomlBin `toml:"bin"`
}

type Properties struct {
	Architecture string `json:"architecture"`
}

func (r *Runtime) Build(ctx context.Context, input *runtime.BuildInput) (*runtime.BuildOutput, error) {
	r.mut.Lock()
	defer r.mut.Unlock()
	var properties Properties
	json.Unmarshal(input.Properties, &properties)

	handlerName := strings.TrimSuffix(filepath.Base(input.Handler), filepath.Ext(input.Handler))
	cargoTomlFile, err := fs.FindUp(input.Handler, "Cargo.toml")
	if err != nil {
		return nil, err
	}

	// root of rust project
	root := filepath.Dir(cargoTomlFile)
	out := input.Out()
	slog.Info("got handler", "HANDLER", input.Handler, "handlerName", handlerName, "out", out)

	var cargoToml CargoToml
	if _, err := toml.DecodeFile(*&cargoTomlFile, &cargoToml); err != nil {
		slog.Error("Error decoding TOML file", "err", err)
	}

	var requiredFeatures []string
	for _, v := range cargoToml.Bin {
		if v.Name == handlerName {
			requiredFeatures = v.RequiredFeatures
			break
		}
	}

	cargoConfigFile := FindClosestCargoConfig(root)

	var cargoConfig CargoConfig
	if cargoConfigFile != nil {
		if _, err := toml.DecodeFile(*cargoConfigFile, &cargoConfig); err != nil {
			slog.Error("Error decoding TOML file", "err", err)
		}
	}

	args := []string{"lambda", "build", "--bin", handlerName}

	if !input.Dev {
		args = append(args, "--release")
	}

	if properties.Architecture == "arm_64" {
		args = append(args, "--arm64")
	}

	args = append(args, "--no-default-features")

	if len(requiredFeatures) > 0 {
		args = append(args, "--features", strings.Join(requiredFeatures, ","))
	}

	cmd := process.Command("cargo", args...)

	env := os.Environ()
	cmd.Dir = root
	cmd.Env = env
	slog.Info("running cargo build", "cmd", cmd.Args)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return &runtime.BuildOutput{
			Errors: []string{string(output)},
		}, nil
	} else {
		var targetPath string

		if cargoConfig.Build.TargetDir != nil {
			targetPath = filepath.Join(root, *cargoConfig.Build.TargetDir)
		} else {
			targetPath = filepath.Join(root, "target")
		}

		src := filepath.Join(targetPath, "lambda", handlerName, "bootstrap")
		dst := filepath.Join(out, "bootstrap")

		copyFile(src, dst)
	}
	r.directories[input.FunctionID], _ = filepath.Abs(root)
	return &runtime.BuildOutput{
		Handler:    "bootstrap",
		Sourcemaps: []string{},
		Errors:     []string{},
		Out:        out,
	}, nil
}

func (r *Runtime) Run(ctx context.Context, input *runtime.RunInput) (runtime.Worker, error) {
	cmd := process.Command(
		filepath.Join(input.Build.Out, input.Build.Handler),
	)
	slog.Info("running cargo", "server", input.Server)
	cmd.Env = input.Env
	cmd.Env = append(cmd.Env, "AWS_LAMBDA_RUNTIME_API="+input.Server)
	cmd.Dir = input.Build.Out
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	cmd.Start()
	return &Worker{
		stdout,
		stderr,
		cmd,
	}, nil
}

func (r *Runtime) ShouldRebuild(functionID string, file string) bool {
	if !strings.HasSuffix(file, ".rs") {
		return false
	}
	match, ok := r.directories[functionID]
	if !ok {
		return false
	}
	slog.Info("checking if file needs to be rebuilt", "file", file, "match", match)
	rel, err := filepath.Rel(match, file)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

// FindClosestCargoConfig traverses up the directory tree to find the closest .cargo/config.toml file.
func FindClosestCargoConfig(startingPath string) *string {
	dir := startingPath
	for {
		cargoDir := filepath.Join(dir, ".cargo")

		if _, err := os.Stat(cargoDir); err == nil {
			cargoConfig := filepath.Join(cargoDir, "config.toml")
			if _, err := os.Stat(cargoConfig); err == nil {
				return &cargoConfig
			}
			cargoConfig = filepath.Join(cargoDir, "config")
			if _, err := os.Stat(cargoConfig); err == nil {
				return &cargoConfig
			}
		}

		// Move up one directory
		parentDir := filepath.Dir(dir)
		if parentDir == dir {
			// Reached the root directory
			break
		}
		dir = parentDir
	}
	return nil
}

func copyFile(src, dst string) error {
	slog.Info("copying bootstrap file", "src", src, "dst", dst)
	// Open the source file
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	// Ensure the destination directory exists
	destDir := filepath.Dir(dst)
	if err := os.MkdirAll(destDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create destination directories for %s: %v", dst, err)
	}

	// Create the destination file
	destinationFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destinationFile.Close()

	// Copy the content from source to destination
	_, err = io.Copy(destinationFile, sourceFile)
	if err != nil {
		return err
	}

	// Flush the writes to stable storage
	err = destinationFile.Sync()
	if err != nil {
		return err
	}

	return nil
}
