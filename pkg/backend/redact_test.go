package backend

import (
	"reflect"
	"testing"
)

func TestRedactSpawnArgs(t *testing.T) {
	in := []string{
		"claude", "--append-system-prompt", "Be concise.",
		"-c", "developer_instructions=\"secret\"",
		"--model", "opus",
	}
	got := RedactSpawnArgs(in)
	want := []string{
		"claude", "--append-system-prompt", "[11 chars]",
		"-c", "developer_instructions=[8 chars]",
		"--model", "opus",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RedactSpawnArgs = %v", got)
	}
	if in[2] != "Be concise." {
		t.Fatal("input argv mutated")
	}
}
