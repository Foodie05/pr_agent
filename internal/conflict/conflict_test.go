package conflict

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type runnerFunc func(context.Context, string, string, ...string) (string, error)

func (f runnerFunc) Run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	return f(ctx, dir, name, args...)
}

func TestExtractConflictBlocksParsesCurrentAndIncomingParts(t *testing.T) {
	content := "line1\n<<<<<<< HEAD\ncurrent a\ncurrent b\n=======\nincoming a\nincoming b\n>>>>>>> main\nline2\n"
	blocks := extractConflictBlocks(content)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].CurrentPart != "current a\ncurrent b\n" {
		t.Fatalf("unexpected current part: %q", blocks[0].CurrentPart)
	}
	if blocks[0].IncomingPart != "incoming a\nincoming b\n" {
		t.Fatalf("unexpected incoming part: %q", blocks[0].IncomingPart)
	}
}

func TestResolveRemainingBlocksPreferCurrent(t *testing.T) {
	content := "before\n<<<<<<< HEAD\nkeep this\n=======\ndrop this\n>>>>>>> main\nafter\n"
	resolved := resolveRemainingBlocksPreferCurrent(content)
	if hasConflictBlocks(resolved) {
		t.Fatalf("expected markers to be removed, got %q", resolved)
	}
	if !strings.Contains(resolved, "keep this") {
		t.Fatalf("expected current side to remain, got %q", resolved)
	}
	if strings.Contains(resolved, "drop this") {
		t.Fatalf("expected incoming side to be removed, got %q", resolved)
	}
}

func TestUnresolvedConflictBlockCountIgnoresStringLiterals(t *testing.T) {
	content := "func demo() string {\n\treturn \"<<<<<<< not a real conflict marker\"\n}\n"
	if unresolvedConflictBlockCount(content) != 0 {
		t.Fatalf("expected string literal marker text to be ignored, got %d", unresolvedConflictBlockCount(content))
	}
}

func TestIsResolvableFileAllowsLargerTextFilesInForceMode(t *testing.T) {
	large := strings.Repeat("a", 41000)
	file := FileConflict{
		Path:            "internal/orchestrator/orchestrator.go",
		BaseContent:     large,
		CurrentContent:  large,
		IncomingContent: large,
		ConflictMarkers: "<<<<<<< HEAD\nleft\n=======\nright\n>>>>>>> main\n",
	}
	if isResolvableFile(file, ModeAutoResolve) {
		t.Fatalf("expected auto resolve mode to reject oversized file")
	}
	if !isResolvableFile(file, ModeForceResolve) {
		t.Fatalf("expected force resolve mode to allow larger text file")
	}
}

func TestForceResolveBlockFallbackPrefersCurrent(t *testing.T) {
	block := conflictBlock{
		CurrentPart:  "keep current\n",
		IncomingPart: "take incoming\n",
	}
	if got := forceResolveBlockFallback(block); got != "keep current\n" {
		t.Fatalf("expected current part, got %q", got)
	}
}

func TestForceResolveBlockFallbackUsesIncomingWhenCurrentEmpty(t *testing.T) {
	block := conflictBlock{
		CurrentPart:  "",
		IncomingPart: "take incoming\n",
	}
	if got := forceResolveBlockFallback(block); got != "take incoming\n" {
		t.Fatalf("expected incoming part, got %q", got)
	}
}

func TestIsIgnorableConflictFile(t *testing.T) {
	if !isIgnorableConflictFile(".DS_Store") {
		t.Fatalf("expected .DS_Store to be ignorable")
	}
	if !isIgnorableConflictFile("ios/.DS_Store") {
		t.Fatalf("expected nested .DS_Store to be ignorable")
	}
	if isIgnorableConflictFile("lib/main.dart") {
		t.Fatalf("expected source file to not be ignorable")
	}
}

func TestResolveIgnorableConflictFileRemovesTrackedArtifact(t *testing.T) {
	repoDir := t.TempDir()
	filePath := filepath.Join(repoDir, ".DS_Store")
	if err := os.WriteFile(filePath, []byte("junk"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	var calls [][]string
	resolver := &GitResolver{
		Runner: runnerFunc(func(_ context.Context, _ string, name string, args ...string) (string, error) {
			call := append([]string{name}, args...)
			calls = append(calls, call)
			return "", nil
		}),
	}

	if err := resolver.resolveIgnorableConflictFile(repoDir, ".DS_Store"); err != nil {
		t.Fatalf("resolve ignorable file: %v", err)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed, got err=%v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "git rm -f --ignore-unmatch .DS_Store" {
		t.Fatalf("unexpected git calls: %v", calls)
	}
}
