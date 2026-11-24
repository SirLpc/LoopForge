package runner

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

//go:embed unity_test_windows.ps1
var windowsUnityTestScript string

//go:embed unity_test_windows_job.cs
var windowsUnityTestJob string

var windowsUnityTestEncodedScript = encodeWarmWindowsScript(windowsUnityTestScript)

type windowsUnityTestRequest struct {
	Editor  string `json:"editor"`
	Project string `json:"project"`
	Mode    string `json:"-"`
	Filter  string `json:"-"`
	Args    string `json:"args"`
	Job     string `json:"job"`
}

type windowsUnityTestOutput struct {
	Status       string `json:"status"`
	Message      string `json:"message"`
	ExitCode     *int   `json:"exitCode"`
	XML          []byte `json:"xml"`
	Log          []byte `json:"log"`
	LogTruncated bool   `json:"logTruncated"`
}

func runWindowsUnityTest(ctx context.Context, request windowsUnityTestRequest, guard func() error) (windowsUnityTestOutput, error) {
	var output windowsUnityTestOutput
	if err := ctx.Err(); err != nil {
		return output, err
	}
	args := []string{"-batchmode", "-runTests", "-projectPath", request.Project, "-testPlatform", request.Mode}
	if request.Filter != "" {
		args = append(args, "-testFilter", request.Filter)
	}
	request.Args = warmWindowsCommandLine(args)
	request.Job = warmWindowsJobSource + "\n" + windowsUnityTestJob
	input, err := json.Marshal(request)
	if err != nil {
		return output, err
	}
	cmd := exec.Command(warmWindowsPowerShell, "-NoProfile", "-NonInteractive", "-EncodedCommand", windowsUnityTestEncodedScript)
	cmd.Dir = "/mnt/c/Windows"
	cmd.Env = []string{"WSL_INTEROP=" + os.Getenv("WSL_INTEROP")}
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return output, err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return output, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return output, fmt.Errorf("start Windows Unity launcher (check WSL interop): %w", err)
	}
	launcherDone := make(chan struct{})
	defer close(launcherDone)
	stop := context.AfterFunc(ctx, func() {
		_ = stdin.Close()
		// The Windows watchdog terminates the job even if PowerShell cannot
		// reach its polling loop. Bound the interop pipe wait separately.
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		select {
		case <-launcherDone:
		case <-timer.C:
			_ = cmd.Process.Kill()
			_ = stdout.Close()
		}
	})
	defer stop()
	// The launcher cannot start Unity until Runner acknowledges its preflight.
	startup := time.AfterFunc(30*time.Second, func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
	})
	defer startup.Stop()
	_, writeErr := stdin.Write(append(input, '\n'))
	if writeErr != nil {
		_ = stdin.Close()
	}
	reader := bufio.NewReader(io.LimitReader(stdout, 17<<20))
	first, readErr := reader.ReadBytes('\n')
	var guardErr error
	if strings.TrimSpace(string(first)) == "loopforge-unity-ready" {
		if !startup.Stop() {
			guardErr = errors.New("Windows Unity launcher preflight timed out")
		} else if guardErr = ctx.Err(); guardErr == nil {
			guardErr = guard()
		}
		if guardErr == nil {
			_, guardErr = stdin.Write([]byte("run\n"))
		}
		if guardErr != nil {
			_ = stdin.Close()
		}
		first = nil
	}
	rest, restErr := io.ReadAll(reader)
	_ = stdout.Close()
	waitErr := cmd.Wait()
	if guardErr != nil {
		return output, guardErr
	}
	if err := errors.Join(writeErr, restErr, waitErr, json.Unmarshal(append(first, rest...), &output)); err != nil {
		if ctx.Err() != nil {
			return output, ctx.Err()
		}
		return output, fmt.Errorf("Windows Unity launcher failed: %w %s", errors.Join(err, readErr), strings.TrimSpace(stderr.String()))
	}
	return output, nil
}
