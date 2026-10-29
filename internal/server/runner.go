package server

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
)

// Runner runs the few fixed commands noded needs. Tests replace it.
type Runner interface {
	// Run returns combined output; err is non-nil on a non-zero exit.
	Run(ctx context.Context, name string, args ...string) (string, error)
	// Stream calls emit for every output line until the command ends or ctx is cancelled.
	Stream(ctx context.Context, emit func(string) error, name string, args ...string) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (ExecRunner) Stream(ctx context.Context, emit func(string) error, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(pipe)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := emit(sc.Text()); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
	}
	_ = cmd.Wait()
	return ctx.Err()
}
