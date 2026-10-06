package lifecycle

import "testing"

func TestHappyPathAndNoSkipping(t *testing.T) {
	tr := New()
	if err := tr.Advance(Profitable, ""); err == nil {
		t.Fatal("skipped SIMULATED")
	}
	for _, s := range []Stage{Simulated, Profitable, EconomicallyValid} {
		if err := tr.Advance(s, ""); err != nil {
			t.Fatal(err)
		}
	}
	tr.Reject(NegativeAfterGas, "net -1 wei")
	if tr.Stage != Rejected || tr.Reason != NegativeAfterGas || tr.LastAccepted() != EconomicallyValid {
		t.Fatalf("bad reject state %+v", tr)
	}
	if tr.Advance(ExecutionFeasible, "") == nil {
		t.Fatal("rejected candidate advanced")
	}
}
