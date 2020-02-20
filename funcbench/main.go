// Copyright 2019 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/oklog/run"
	"github.com/pkg/errors"
	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
)

type Logger interface {
	Println(v ...interface{})
}

type logger struct {
	*log.Logger

	verbose bool
}

func (l *logger) FatalError(err error) {
	if l.verbose {
		l.Fatalf("%+v", err)
	}
	l.Fatalf("%v", err)
}

func main() {
	var err error

	app := kingpin.New(filepath.Base(os.Args[0]), "benchmark result posting and formating tool.")
	app.HelpFlag.Short('h')
	isLocal := app.Flag("local", "Local CLI mode").Short('l').Bool()
	eventFilePath := app.Flag("input", "Location of the GitHub hook file (e.g event.json). Ignored when local mode is enabled.").
		Short('i').Default("/github/workflow/event.json").String()
	isRaceDisabled := app.Flag("no-race", "Disables -race").Bool()
	verbose := app.Flag("verbose", "Verbose mode.").Short('v').Bool()
	// Allow specifying this via github.
	benchTime := app.Flag("time", "-bench-time").Short('t').Duration()
	resultCacheDir := app.Flag("result-cache", "Directory to store output for given func name and commit sha. Useful for local runs.").String()

	compareTarget := app.Arg("branch/commit/<.>", "Branch, commit SHA of the branch to compare benchmarks against. If `.` of branch/commit is the same as the current one, funcbench will run once and try to compare between 2 sub-benchmarks.").String()
	benchFunc := app.Arg("func regexp/<.>", "Function to use for benchmark. Supports RE2 regexp or `.` to run all benchmarks.").String()

	kingpin.MustParse(app.Parse(os.Args[1:]))

	logger := &logger{
		// Show file line with each log.
		Logger:  log.New(os.Stdout, "funcbech", log.Ltime|log.Lshortfile),
		verbose: *verbose,
	}
	var g run.Group

	// Main routine.
	{
		ctx, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			c := &commander{verbose: *verbose}

			var env Environment
			if *isLocal {
				logger.Println("Local mode assumed.")
				env, err = newLocalEnv(environment{
					logger:        logger,
					benchFunc:     *benchFunc,
					compareTarget: *compareTarget,
					isRaceEnabled: !*isRaceDisabled,
					home:          os.Getenv("HOME"),
				})
			} else {
				logger.Println("GitHub environment assumed. Event file:", eventFilePath)
				if os.Getenv("GITHUB_WORKSPACE") == "" {
					return errors.New("am I really inside GitHub action?")
				}

				if *compareTarget != "" || *benchFunc != "" || *isRaceDisabled {
					return errors.New("both --input and args specified. This is not allowed. Args will be read from --input file.")
				}
				env, err = newGitHubEnv(ctx, logger, *eventFilePath)
			}
			if err != nil {
				return errors.Wrap(err, "new env")
			}

			bench, err := newBenchmarker(logger, env, c, *benchTime, *resultCacheDir)
			if err != nil {
				return errors.Wrap(err, "create benchmarker")
			}

			// Execute benchmark against packages in the current directory.
			wt, _ := env.Repo().Worktree()
			ref, err := env.Repo().Head()
			if err != nil {
				return errors.Wrap(err, "get head")
			}
			newResult, err := bench.execBenchmark(wt.Filesystem.Root(), ref.Hash())
			if err != nil {
				// TODO(bwplotka): Just defer posting all errors?
				if pErr := env.PostErr("Go bench test for this pull request failed"); pErr != nil {
					return errors.Errorf("error: %v occured while processing error: %v", pErr, err)
				}
				return errors.Wrap(err, "exec benchmark A")
			}

			var oldResult string

			targetCommit, isLocal, err := checkout(ctx, logger, env.Repo(), env.CompareTarget(), c)
			if err != nil {
				return errors.Wrap(err, "checkout")
			}

			var cmps []BenchCmp
			if isLocal {
				cmps, err = bench.compareSubBenchmarks(newResult)
				if err != nil {
					if pErr := env.PostErr("`benchcmp` failed."); pErr != nil {
						return errors.Errorf("error: %v occured while processing error: %v", pErr, err)
					}
					return errors.Wrap(err, "compare sub benchmarks")
				}
			} else {
				oldResult, err = bench.execBenchmark(worktreeDir(env.Repo()), targetCommit)
				if err != nil {
					if pErr := env.PostErr(fmt.Sprintf("Go bench test for target %s failed", env.CompareTarget())); pErr != nil {
						return errors.Errorf("error: %v occured while processing error: %v", pErr, err)
					}
					return errors.Wrap(err, "bench")
				}

				cmps, err = bench.compareBenchmarks(oldResult, newResult)
				if err != nil {
					if pErr := env.PostErr("`benchcmp` failed."); pErr != nil {
						return errors.Errorf("error: %v occured while processing error: %v", pErr, err)
					}
					return errors.Wrap(err, "compare benchmarks")
				}
			}

			return errors.Wrap(env.PostResults(cmps), "post results")
		}, func(err error) {
			cancel()
		})
	}
	// Listen for termination signals.
	{
		cancel := make(chan struct{})
		g.Add(func() error {
			return interrupt(logger, cancel)
		}, func(error) {
			close(cancel)
		})
	}

	if err := g.Run(); err != nil {
		logger.FatalError(errors.Wrap(err, "running command failed"))
	}
	logger.Println("exiting")
}

func interrupt(logger Logger, cancel <-chan struct{}) error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-c:
		logger.Println("caught signal", s, "Exiting.")
		return nil
	case <-cancel:
		return errors.New("canceled")
	}
}

func worktreeDir(repo *git.Repository) string {
	wt, _ := repo.Worktree()
	return filepath.Join(wt.Filesystem.Root(), ".funcbench-cmp")
}

func checkout(ctx context.Context, logger Logger, repo *git.Repository, target string, c *commander) (ref plumbing.Hash, isCurrent bool, _ error) {
	currRef, err := repo.Head()
	if err != nil {
		return plumbing.ZeroHash, false, err
	}

	switch {
	case target == ".":
		fallthrough
	case target == strings.TrimPrefix(currRef.Name().String(), "refs/heads/"):
		fallthrough
	case target == currRef.Hash().String():
		logger.Println("Target:", target, "is `.` or the same as current ref:", currRef.String(), "; Assuming sub-benchmarks comparison.")
		return currRef.Hash(), true, nil
	}
	if err := repo.FetchContext(ctx, &git.FetchOptions{}); err != nil && err != git.NoErrAlreadyUpToDate {
		return plumbing.ZeroHash, false, err
	}

	targetRef, err := repo.Reference(plumbing.NewBranchReferenceName(target), false)
	if err != nil {
		return plumbing.ZeroHash, false, nil
	}
	fmt.Println("Comparing benchmarks against:", targetRef.String(), "(clean workdir is mandatory)")

	// Best effort.
	_, _ = c.exec("git", "worktree", "remove", worktreeDir(repo))

	_, err = c.exec("git", "worktree", "add", "-f", worktreeDir(repo), target)
	return targetRef.Hash(), false, err
}

type commander struct {
	verbose bool
}

func (c *commander) exec(command ...string) (string, error) {
	// TODO(bwplotka): Use context to kill command on interrupt.
	cmd := exec.Command(command[0], command[1:]...)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b

	if c.verbose {
		// All to stdout.
		cmd.Stdout = io.MultiWriter(cmd.Stdout, os.Stdout)
		cmd.Stderr = io.MultiWriter(cmd.Stdout, os.Stdout)
	}
	if err := cmd.Run(); err != nil {
		out := b.String()
		if c.verbose {
			out = ""
		}
		return "", errors.Errorf("error: %v; Command out: %s", err, out)
	}

	return b.String(), nil
}
