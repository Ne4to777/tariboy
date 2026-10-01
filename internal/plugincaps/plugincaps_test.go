package plugincaps

import (
	"reflect"
	"testing"
)

func TestResolveWithExternal(t *testing.T) {
	got, err := ResolveWithExternal([]string{"context", "status"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"whoami", "loop", "messages", "context", "status"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveWithExternal = %v, want %v", got, want)
	}
	// dedupe: requesting a CORE plugin does not duplicate it
	got, _ = ResolveWithExternal([]string{"whoami", "context"}, nil)
	if !reflect.DeepEqual(got, []string{"whoami", "loop", "messages", "context"}) {
		t.Fatalf("dedupe failed: %v", got)
	}
	if _, err := ResolveWithExternal([]string{"nope"}, nil); err == nil {
		t.Fatal("unknown plugin accepted")
	}
	if _, err := ResolveWithExternal([]string{"be" + "ads"}, nil); err == nil {
		t.Fatal("retired plugin accepted")
	}
}

func TestWorkdirIsV2InstructionPluginOnly(t *testing.T) {
	if _, err := ResolveWithExternal([]string{"workdir"}, nil); err == nil {
		t.Fatal("schema-v1 resolution accepted workdir")
	}
	got, err := ValidateExplicit([]string{"workdir"}, nil)
	if err != nil {
		t.Fatalf("ValidateExplicit(workdir): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"workdir"}) {
		t.Fatalf("ValidateExplicit(workdir) = %v", got)
	}
	if IsOptional("workdir") {
		t.Fatal("instruction-only workdir reported as an optional capability")
	}
}

func TestIsOptional(t *testing.T) {
	if IsOptional("whoami") {
		t.Fatal("core plugin reported optional")
	}
	if !IsOptional("context") {
		t.Fatal("context should be optional")
	}
}

func TestImageCreatorCapability(t *testing.T) {
	if !IsOptional("image-creator") {
		t.Fatal("image-creator must be an OPTIONAL capability")
	}
	resolved, err := ResolveWithExternal([]string{"image-creator"}, nil)
	if err != nil {
		t.Fatalf("ResolveWithExternal: %v", err)
	}
	found := false
	for _, n := range resolved {
		if n == "image-creator" {
			found = true
		}
	}
	if !found {
		t.Fatalf("image-creator missing from resolved set %v", resolved)
	}
}

func TestTasksCapabilityIsOptionalAndContributesItsOwnPrompt(t *testing.T) {
	if !IsOptional("tasks") {
		t.Fatal("tasks must be an OPTIONAL built-in capability")
	}
	resolved, err := ResolveWithExternal([]string{"tasks"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range resolved {
		if candidate == "tasks" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tasks missing from resolved set %v", resolved)
	}
	without, err := ResolveWithExternal(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range without {
		if candidate == "tasks" {
			t.Fatal("tasks prompt appears when capability is disabled")
		}
	}
}

func TestGoalCapabilityReplacesCurrentTask(t *testing.T) {
	if _, err := ValidateExplicit([]string{"goal"}, nil); err != nil {
		t.Fatalf("goal plugin validation: %v", err)
	}
	if _, err := ValidateExplicit([]string{"current-task"}, nil); err == nil {
		t.Fatal("current-task plugin still validates")
	}
	if !IsOptional("goal") {
		t.Fatal("goal is not a capability")
	}
}
