package extensions

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const extensionProcessFixtureEnv = "SPYNEL_EXTENSION_PROCESS_FIXTURE"

func TestExtensionProcessFixture(t *testing.T) {
	if os.Getenv(extensionProcessFixtureEnv) != "fail-empty-stderr" {
		return
	}
	os.Exit(9)
}

func TestFailedHookWithoutStderrStillWritesProcessEvidence(t *testing.T) {
	root := t.TempDir()
	extension := filepath.Join(root, "silent-failure")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, err := yaml.Marshal(Manifest{Name: "silent-failure", Hooks: map[string][]string{
		"message.received": {os.Args[0], "-test.run=^TestExtensionProcessFixture$"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, ManifestName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(extensionProcessFixtureEnv, "fail-empty-stderr")
	var diagnostic bytes.Buffer
	_, runErr := (Runner{Directory: root, Timeout: 5 * time.Second, Log: &diagnostic}).Run(context.Background(), "message.received", map[string]any{"text": "not logged"})
	if runErr == nil {
		t.Fatal("silent nonzero hook unexpectedly succeeded")
	}
	got := diagnostic.String()
	if !strings.Contains(got, "process_failed=exit status 9") || !strings.Contains(got, "stderr_present=false") {
		t.Fatalf("silent process evidence = %q", got)
	}
	if strings.Contains(got, "not logged") {
		t.Fatalf("hook input leaked into diagnostic: %q", got)
	}
}

func TestBoundedHookBufferAcceptsWithoutGrowingPastLimit(t *testing.T) {
	buffer := boundedBuffer{limit: 8}
	data := []byte("0123456789abcdef")
	written, err := buffer.Write(data)
	if err != nil || written != len(data) || buffer.String() != "01234567" || !buffer.truncated {
		t.Fatalf("bounded write = %d, %v, %q, truncated=%t", written, err, buffer.String(), buffer.truncated)
	}
	var output bytes.Buffer
	_, _ = output.Write(buffer.Bytes())
	if output.Len() != 8 {
		t.Fatalf("bounded output length = %d", output.Len())
	}
}

func TestHookCanRewritePayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	root := t.TempDir()
	extension := filepath.Join(root, "rewrite")
	if err := os.MkdirAll(extension, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "name: rewrite\nhooks:\n  message.received: [\"./hook.sh\"]\n"
	script := "#!/bin/sh\nread input\nprintf '%s\\n' '{\"payload\":{\"text\":\"rewritten\"}}'\n"
	if err := os.WriteFile(filepath.Join(extension, ManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "hook.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{Directory: root, Timeout: time.Second}).Run(context.Background(), "message.received", map[string]any{"text": "original"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Payload["text"] != "rewritten" {
		t.Fatalf("hook payload = %#v", result.Payload)
	}
}

func TestDiscoveryRejectsUnsupportedHooks(t *testing.T) {
	for _, hook := range []string{"update.before", "task.claimed", "task.completed"} {
		t.Run(hook, func(t *testing.T) {
			root := t.TempDir()
			extension := filepath.Join(root, "stale")
			if err := os.MkdirAll(extension, 0o700); err != nil {
				t.Fatal(err)
			}
			manifest := "name: stale\nhooks:\n  " + hook + ": [\"./hook.sh\"]\n"
			if err := os.WriteFile(filepath.Join(extension, ManifestName), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := (Runner{Directory: root}).Run(context.Background(), "message.received", nil)
			if err == nil || !strings.Contains(err.Error(), `unsupported hook "`+hook+`"`) {
				t.Fatalf("unsupported hook error = %v", err)
			}
		})
	}
}
