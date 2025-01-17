// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package harnesses

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/benchmarks/sweet/common"
	"golang.org/x/benchmarks/sweet/common/log"
)

// CockroachDB implements the Harness interface.
type CockroachDB struct{}

func (h CockroachDB) CheckPrerequisites() error {
	// Cockroachdb is only supported on arm64 and amd64 architectures.
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		return fmt.Errorf("requires amd64 or arm64")
	}
	return nil
}

func (h CockroachDB) Get(gcfg *common.GetConfig) error {
	// Build against a commit that includes https://github.com/cockroachdb/cockroach/pull/125588.
	// Recursive clone the repo as we need certain submodules, i.e.
	// PROJ, for the build to work.
	return gitRecursiveCloneToCommit(
		gcfg.SrcDir,
		"https://github.com/cockroachdb/cockroach",
		"master",
		"c4a0d997e0da6ba3ebede61b791607aa452b9bbc",
	)
}

func (h CockroachDB) Build(cfg *common.Config, bcfg *common.BuildConfig) error {
	// Build the cockroach binary.
	// We do this by using the cockroach `dev` tool. The dev tool is a bazel
	// wrapper normally used for building cockroach, but can also be used to
	// generate artifacts that can then be built by `go build`.

	// Install bazel via bazelisk which is used by `dev`. Install it in the
	// BinDir to ensure we get a new copy every run and avoid reuse. This is
	// done by setting the `GOBIN` env var for the `go install` cmd.
	goInstall := cfg.GoTool()
	goInstall.Env = goInstall.Env.MustSet(fmt.Sprintf("GOBIN=%s", bcfg.BinDir))
	if err := goInstall.Do(bcfg.BinDir, "install", "github.com/bazelbuild/bazelisk@latest"); err != nil {
		return fmt.Errorf("error building bazelisk: %v", err)
	}

	// Helper that returns the path to the bazel binary.
	bazel := func() string {
		return filepath.Join(bcfg.BinDir, "bazelisk")
	}

	// Clean up the bazel workspace. If we don't do this, our _bazel directory
	// will quickly grow as Bazel treats each run as its own workspace with its
	// own artifacts.
	defer func() {
		cmd := exec.Command(bazel(), "clean", "--expunge")
		cmd.Dir = bcfg.SrcDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		// Cleanup is best effort, there might not be anything to clean up
		// if we fail early enough in the build process.
		_ = cmd.Run()
	}()

	// Configure the build env.
	env := cfg.BuildEnv.Env
	env = env.Prefix("PATH", filepath.Join(cfg.GoRoot, "bin")+":")
	env = env.MustSet("GOROOT=" + cfg.GoRoot)

	// Use bazel to generate the artifacts needed to enable a `go build`.
	cmd := exec.Command(bazel(), "run", "//pkg/gen:code")
	cmd.Dir = bcfg.SrcDir
	cmd.Env = env.Collapse()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	// Build the c-deps needed.
	cmd = exec.Command(bazel(), "run", "//pkg/cmd/generate-cgo:generate-cgo", "--run_under", fmt.Sprintf("cd %s && ", bcfg.SrcDir))
	cmd.Dir = bcfg.SrcDir
	cmd.Env = env.Collapse()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	// Finally build the cockroach binary with `go build`. Build the
	// cockroach-short binary as it is functionally the same, but
	// without the UI, making it much quicker to build.
	//
	// As of go1.23, we need to pass the `-ldflags=-checklinkname=0` flag
	// to build cockroach. However, benchmark release branches are on older
	// versions that don't recognize the flag. Try first with the flag and
	// again without if there is an error.
	if buildWithFlagErr := cfg.GoTool().BuildPath(filepath.Join(bcfg.SrcDir, "pkg/cmd/cockroach-short"), bcfg.BinDir, "-ldflags=-checklinkname=0"); buildWithFlagErr != nil {
		if buildWithoutFlagErr := cfg.GoTool().BuildPath(filepath.Join(bcfg.SrcDir, "pkg/cmd/cockroach-short"), bcfg.BinDir); buildWithoutFlagErr != nil {
			return errors.Join(buildWithFlagErr, buildWithoutFlagErr)
		}
	}

	// Rename the binary from cockroach-short to cockroach for
	// ease of use.
	if err := copyFile(filepath.Join(bcfg.BinDir, "cockroach"), filepath.Join(bcfg.BinDir, "cockroach-short")); err != nil {
		return err
	}

	// Build the benchmark wrapper.
	if err := cfg.GoTool().BuildPath(bcfg.BenchDir, filepath.Join(bcfg.BinDir, "cockroachdb-bench")); err != nil {
		return err
	}

	return nil
}

func (h CockroachDB) Run(cfg *common.Config, rcfg *common.RunConfig) error {
	benchmarks := []string{"kv0/nodes=1", "kv50/nodes=1", "kv95/nodes=1", "kv0/nodes=3", "kv50/nodes=3", "kv95/nodes=3"}
	if rcfg.Short {
		benchmarks = []string{"kv0/nodes=3", "kv95/nodes=3"}
	}

	for _, bench := range benchmarks {
		args := append(rcfg.Args, []string{
			"-bench", bench,
			"-cockroachdb-bin", filepath.Join(rcfg.BinDir, "cockroach"),
			"-tmp", rcfg.TmpDir,
		}...)
		if rcfg.Short {
			args = append(args, "-short")
		}
		// The short benchmarks take about 1 minute to run.
		// The long benchmarks take about 10 minutes to run.
		// We set the timeout to 30 minutes to give ample buffer.
		cmd := exec.Command(
			filepath.Join(rcfg.BinDir, "cockroachdb-bench"),
			args...,
		)
		cmd.Env = cfg.ExecEnv.Collapse()
		cmd.Stdout = rcfg.Results
		cmd.Stderr = rcfg.Results
		log.TraceCommand(cmd, false)
		if err := cmd.Start(); err != nil {
			return err
		}
		if rcfg.Short {
			if err := cmd.Wait(); err != nil {
				return err
			}
		} else {
			// Wait for 30 minutes.
			c := make(chan error)
			go func() {
				c <- cmd.Wait()
			}()
			select {
			case err := <-c:
				if err != nil {
					return err
				}
			case <-time.After(30 * time.Minute):
				if err := cmd.Process.Kill(); err != nil {
					return fmt.Errorf("timeout, error killing process: %s", err.Error())
				}
				return fmt.Errorf("timeout")
			}
		}

		// Delete tmp because cockroachdb will have written something there and
		// might attempt to reuse it. We don't want to reuse the same cluster.
		if err := rmDirContents(rcfg.TmpDir); err != nil {
			return err
		}
	}
	return nil
}
