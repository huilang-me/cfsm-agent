package cfprobe

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestCommandOutputHelperProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "cf-probe-command-test" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "wait":
		time.Sleep(time.Minute)
	case "success":
		_, _ = os.Stdout.WriteString("  counter output\n")
	case "failure":
		_, _ = os.Stdout.WriteString("incomplete counters")
		os.Exit(1)
	}
	os.Exit(0)
}

func TestCommandOutputTimeout(t *testing.T) {
	started := time.Now()
	_, err := commandOutputWithTimeout(200*time.Millisecond, os.Args[0], "-test.run=^TestCommandOutputHelperProcess$", "--", "cf-probe-command-test", "wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("timed out command did not stop promptly")
	}
}

func TestCommandOutputSuccessAndFailure(t *testing.T) {
	for _, mode := range []string{"success", "failure"} {
		t.Run(mode, func(t *testing.T) {
			out, err := commandOutputWithTimeout(5*time.Second, os.Args[0], "-test.run=^TestCommandOutputHelperProcess$", "--", "cf-probe-command-test", mode)
			if mode == "success" {
				if err != nil || out != "counter output" {
					t.Fatalf("output=%q error=%v", out, err)
				}
			} else if err == nil || out != "" {
				t.Fatalf("failed command returned usable output: %q, %v", out, err)
			}
		})
	}
}
