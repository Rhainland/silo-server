package nodepool

import "testing"

func TestMarkTranscodeNodeUnreachableStopsSelectionUntilHealthRecovers(t *testing.T) {
	f := newFixture(nil, []*Node{
		transcodeNode(1, "http://dead:8080/", nil, 0),
		transcodeNode(2, "http://alive:8080", nil, 5),
	})

	if !f.planner.MarkTranscodeNodeUnreachable("http://dead:8080") {
		t.Fatal("marking a healthy node reported no change")
	}
	if f.planner.MarkTranscodeNodeUnreachable("http://dead:8080") {
		t.Fatal("marking an already unhealthy node reported a change")
	}
	if f.planner.TranscodeNodeHealthy("http://dead:8080") {
		t.Fatal("marked node still reported healthy")
	}
	// The dead node carries fewer jobs, so only the mark keeps it from winning.
	plan := f.planner.PlanTranscodeSessionWithLocalEgress("session-1", "", nil)
	if plan.TranscodeNode == nil || plan.TranscodeNode.ID != 2 {
		t.Fatalf("selected %+v, want the reachable node", plan.TranscodeNode)
	}

	// The next health sweep is authoritative: a node that answers again is
	// selectable again without any other intervention.
	f.transcodes.ApplyHealth(1, "http://dead:8080/", true, 0, 0, "", nil, nil, f.now)
	if !f.planner.TranscodeNodeHealthy("http://dead:8080") {
		t.Fatal("a healthy sweep result did not clear the mark")
	}
	if f.planner.MarkTranscodeNodeUnreachable("http://unknown:8080") {
		t.Fatal("marking an unpooled node reported a change")
	}
}

// A move that lost its session to a replan releases only its own
// reservation: the replan's reservation, stored under the same session id on
// another node, stands.
func TestReleaseSessionOnTranscodeNodeKeepsAnotherNodesReservation(t *testing.T) {
	f := newFixture(nil, []*Node{transcodeNode(1, "http://only:8080", nil, 0)})
	plan := f.planner.PlanTranscodeSessionWithLocalEgress("session-1", "", nil)
	if plan.TranscodeNode == nil {
		t.Fatal("no node reserved")
	}
	f.planner.ReleaseSessionOnTranscodeNode("session-1", "http://other:8080")
	if _, ok := f.planner.reserved["session-1"]; !ok {
		t.Fatal("releasing another node's reservation dropped this one")
	}
	f.planner.ReleaseSessionOnTranscodeNode("session-1", "http://only:8080/")
	if _, ok := f.planner.reserved["session-1"]; ok {
		t.Fatal("the reservation on the named node was kept")
	}
}
