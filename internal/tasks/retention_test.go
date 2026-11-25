package tasks

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestReclaimedOutputPreservesReceiptAndCursor(t *testing.T) {
	t.Parallel()
	j := journal{root: t.TempDir()}
	const id = "test_run"
	for _, path := range []string{j.run(id, "stdout"), j.run(id, "stderr")} {
		if err := mkdir(path); err != nil {
			t.Fatal(err)
		}
	}
	finished := time.Now().Add(-2 * time.Hour)
	receipt := result{State: "failed", Finished: finished}
	code := 17
	receipt.ExitCode = &code
	if err := writeJSON(j.run(id, "request.json"), taskRequest{BootID: "current", CreatedAt: finished}); err != nil {
		t.Fatal(err)
	}
	if err := appendOutput(j, id, "stdout", &receipt.Stdout, []byte("expired")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(j.run(id, "result.json"), receipt); err != nil {
		t.Fatal(err)
	}
	bytes, err := j.reclaimOutput("current", time.Hour, 0)
	if err != nil || bytes != 0 {
		t.Fatalf("reclamation retained %d bytes, err = %v", bytes, err)
	}
	if _, err := os.Stat(j.run(id, "stdout", "0.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stdout segment remained after pruning: %v", err)
	}
	var stored result
	if err := readJSON(j.run(id, "result.json"), &stored); err != nil || stored.ExitCode == nil || *stored.ExitCode != code {
		t.Fatalf("durable result not preserved: %+v, %v", stored, err)
	}
	task, err := j.retainedOutput(Task{RunID: id, State: stored.State, Stdout: stored.Stdout})
	if err != nil {
		t.Fatal(err)
	}
	out, err := j.readOutput(task, "stdout", 1, 16)
	if err != nil || !out.Gap || !out.Complete || out.RetainedFrom != 7 || out.Next != 7 || len(out.Data) != 0 {
		t.Fatalf("unexpected cursor after reclamation: %+v, %v", out, err)
	}
}
