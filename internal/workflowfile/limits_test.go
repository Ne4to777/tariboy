package workflowfile

import (
	"testing"
	"time"
)

func TestStatusLimits(t *testing.T) {
	f := validFixture(t)
	f.Limits = nil
	if got, want := f.StatusLimits("plan"), (EffectiveLimits{3, 5, 3, 5 * time.Minute}); got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	idle, rej, fail := 7, 8, 9
	f.Limits = &Limits{IdleIterations: &idle, UnavailableGrace: "10m"}
	if got, want := f.StatusLimits("plan"), (EffectiveLimits{7, 5, 3, 10 * time.Minute}); got != want {
		t.Fatalf("workflow override = %+v, want %+v", got, want)
	}
	f.Statuses[0].Limits = &Limits{RejectedRequests: &rej, ScriptFailures: &fail, UnavailableGrace: "0"}
	if got, want := f.StatusLimits("plan"), (EffectiveLimits{7, 8, 9, 0}); got != want {
		t.Fatalf("status override = %+v, want %+v", got, want)
	}
	if got, want := f.StatusLimits("approval"), (EffectiveLimits{7, 5, 3, 10 * time.Minute}); got != want {
		t.Fatalf("other status = %+v, want %+v", got, want)
	}
	if got := f.StatusLimits("nope"); got != f.StatusLimits("approval") {
		t.Fatalf("unknown status = %+v", got)
	}
}
