package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	unityTestClientName = "loopforge-unity-test"
	unityTestSocket     = "test.sock"
	unityTestRequestMax = 16 << 10
)

type unityTestRequest struct {
	Mode       string `json:"mode"`
	Repository string `json:"repository,omitempty"`
	TestFilter string `json:"testFilter,omitempty"`
}

func (r unityTestRequest) validate() error {
	if r.Mode != "EditMode" && r.Mode != "PlayMode" {
		return errors.New("Unity test mode must be EditMode or PlayMode")
	}
	if len(r.Repository) > 256 || len(r.TestFilter) > 2048 ||
		strings.HasPrefix(strings.TrimSpace(r.TestFilter), "-") ||
		strings.ContainsFunc(r.TestFilter+r.Repository, unicode.IsControl) {
		return errors.New("invalid Unity test repository or filter")
	}
	return nil
}

type unityTestResult struct {
	Status       string           `json:"status"`
	Mode         string           `json:"mode,omitempty"`
	Repository   string           `json:"repository,omitempty"`
	Commit       string           `json:"commit,omitempty"`
	Dirty        *bool            `json:"dirty,omitempty"`
	ExitCode     *int             `json:"exitCode,omitempty"`
	Counts       *unityTestCounts `json:"counts,omitempty"`
	XMLPath      string           `json:"xmlPath,omitempty"`
	LogPath      string           `json:"logPath,omitempty"`
	LogTruncated bool             `json:"logTruncated,omitempty"`
	Message      string           `json:"message,omitempty"`
}

type unityTestCounts struct {
	Total        int `json:"total" xml:"total,attr"`
	Passed       int `json:"passed" xml:"passed,attr"`
	Failed       int `json:"failed" xml:"failed,attr"`
	Skipped      int `json:"skipped" xml:"skipped,attr"`
	Inconclusive int `json:"inconclusive" xml:"inconclusive,attr"`
}

func unityReportStatus(data []byte, exitCode int) (string, *unityTestCounts) {
	var report struct {
		XMLName xml.Name `xml:"test-run"`
		Result  string   `xml:"result,attr"`
		unityTestCounts
	}
	if err := xml.Unmarshal(data, &report); err != nil || report.Result == "" {
		return "infra_error", nil
	}
	counts := &report.unityTestCounts
	switch {
	case counts.Failed > 0 || counts.Inconclusive > 0:
		return "failed", counts
	case counts.Passed == 0 && (counts.Total == 0 || report.Result == "Skipped"):
		return "no_tests", counts
	case report.Result == "Passed" && counts.Passed > 0 && exitCode == 0:
		return "passed", counts
	default:
		return "infra_error", counts
	}
}

// RunUnityTestCLI is the task-only entry point of the same runner binary.
func RunUnityTestCLI(argv []string) (int, bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != unityTestClientName {
		return 0, false
	}
	return runUnityTestClient(filepath.Dir(argv[0]), argv[1:], os.Stdout, os.Stderr), true
}

func runUnityTestClient(dir string, args []string, stdout, stderr io.Writer) int {
	var request unityTestRequest
	flags := flag.NewFlagSet(unityTestClientName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&request.Mode, "mode", "", "EditMode or PlayMode")
	flags.StringVar(&request.Repository, "repository", "", "repository ID (required with multiple Unity projects)")
	flags.StringVar(&request.TestFilter, "test-filter", "", "Unity Test Framework test filter")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil || flags.NArg() != 0 {
		return 2
	}
	if err := request.validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, unityTestSocket), 5*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, "Unity test bridge is unavailable for this task")
		return 2
	}
	defer conn.Close()
	var result unityTestResult
	// A busy bridge can reply and close before this client finishes writing.
	_ = json.NewEncoder(conn).Encode(request)
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&result); err != nil {
		fmt.Fprintln(stderr, "Unity test bridge closed without a result")
		return 2
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return 2
	}
	if result.Status == "passed" {
		return 0
	}
	return 1
}

func startUnityTestBridge(ctx context.Context, run func(context.Context, unityTestRequest) unityTestResult) (string, func(), error) {
	dir, err := os.MkdirTemp("/tmp", "lf-unity-")
	if err != nil {
		return "", nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, unityTestSocket), Net: "unix"})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		var handlers sync.WaitGroup
		defer handlers.Wait()
		active := make(chan struct{}, 1)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			select {
			case active <- struct{}{}:
				handlers.Go(func() {
					defer func() { <-active }()
					serveUnityTestRequest(ctx, conn, run)
				})
			default:
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				_ = json.NewEncoder(conn).Encode(unityTestResult{Status: "busy", Message: "this task already has a Unity test in progress"})
				_ = conn.Close()
			}
		}
	}()
	return dir, sync.OnceFunc(func() {
		cancel()
		_ = listener.Close()
		stop()
		<-done
		_ = os.RemoveAll(dir)
	}), nil
}

func serveUnityTestRequest(ctx context.Context, conn net.Conn, run func(context.Context, unityTestRequest) unityTestResult) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, unityTestRequestMax)
	line, readErr := reader.ReadSlice('\n')
	var request unityTestRequest
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	err := errors.Join(readErr, decoder.Decode(&request), request.validate())
	if err != nil || decoder.Decode(new(any)) != io.EOF {
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = json.NewEncoder(conn).Encode(unityTestResult{Status: "infra_error", Message: "invalid Unity test request"})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = reader.ReadByte()
		cancel()
	}()
	result := run(callCtx, request)
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = json.NewEncoder(conn).Encode(result)
}
