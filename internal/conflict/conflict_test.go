package conflict

import (
	"strings"
	"testing"
)

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
