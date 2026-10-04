package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/noderouting"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

const rehomeTestTransport = "rehome-transport"

// fakeRehomeNode is a transcode node that accepts a start and then serves the
// started transport's segments.
type fakeRehomeNode struct {
	server  *httptest.Server
	starts  atomic.Int32
	release chan struct{} // when non-nil, a start waits for it to close

	mu       sync.Mutex
	lastSeen transcodenode.TranscodeStartRequest
}

func newFakeRehomeNode(t *testing.T, gated bool) *fakeRehomeNode {
	t.Helper()
	node := &fakeRehomeNode{}
	if gated {
		node.release = make(chan struct{})
	}
	node.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/transcode/start":
			var req transcodenode.TranscodeStartRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			node.mu.Lock()
			node.lastSeen = req
			node.mu.Unlock()
			node.starts.Add(1)
			if node.release != nil {
				<-node.release
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(transcodenode.TranscodeStartResponse{SessionID: req.SessionID, Status: "started"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/transcode/"+rehomeTestTransport+"/segment/"):
			_, _ = io.WriteString(w, "moved:"+strings.TrimPrefix(r.URL.Path, "/transcode/"+rehomeTestTransport+"/segment/"))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(node.server.Close)
	return node
}

func (n *fakeRehomeNode) lastStart() transcodenode.TranscodeStartRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastSeen
}

// unreachableNodeURL returns a URL nothing listens on, so a dial is refused.
func unreachableNodeURL(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	return url
}

type rehomeFixture struct {
	handler  *PlaybackHandler
	sessions *playback.SessionManager
	planner  *nodepool.Planner
	session  *playback.Session
	token    string
	deadURL  string
}

// newRehomeFixture starts a native API-relayed transcode on nodes[0] and pools
// every node in nodes.
func newRehomeFixture(t *testing.T, nodes []*nodepool.Node, ffmpegPath string) *rehomeFixture {
	t.Helper()
	pool := nodepool.NewTranscodePool()
	pool.SetNodes(nodes)
	planner := nodepool.NewPlanner(nodepool.NewProxyPool(), pool)

	sessions := playback.NewSessionManager(0, 0)
	handler := NewPlaybackHandler(sessions)
	handler.JWTSecret = "rehome-secret"
	handler.NodePlanner = planner
	handler.PlaybackConfig = playbackTestConfig(ffmpegPath, t.TempDir())

	session, err := sessions.StartSession(1, "profile-1", 42, playback.PlayTranscode, false)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	t.Cleanup(func() {
		if ts := handler.tm.GetTranscodeSession(session.ID); ts != nil {
			_ = ts.Close()
		}
	})
	deadURL := nodes[0].URL
	if err := sessions.SetTranscodeRoute(session.ID, playback.TranscodeRoute{NodeURL: deadURL, TransportID: rehomeTestTransport}); err != nil {
		t.Fatal(err)
	}
	if err := sessions.SetNodeRoutingAssignment(session.ID, playback.NodeRoutingAssignment{
		Workload: string(noderouting.WorkloadVideoTranscode), Execution: string(noderouting.ExecutionTranscode),
		ExecutionNodeID: nodes[0].ID, ExecutionNodeURL: deadURL, Egress: string(noderouting.EgressAPI),
	}); err != nil {
		t.Fatal(err)
	}
	card := playback.NewRecipeCard(1, "profile-1", 42, deadURL, playback.TranscodeOpts{
		SessionID: session.ID, TranscodeTransportID: rehomeTestTransport, InputPath: "/media/movie.mkv",
		TargetCodecVideo: "h264", TargetCodecAudio: "aac", SegmentDuration: 2,
		AudioTrackIndex: -1, SubtitleTrackIndex: -1, TotalDuration: 600,
	})
	card.RoutingWorkload = string(noderouting.WorkloadVideoTranscode)
	card.RoutingExecution = string(noderouting.ExecutionTranscode)
	card.RoutingExecutionNodeID = nodes[0].ID
	card.RoutingEgress = string(noderouting.EgressAPI)
	token := handler.signSessionToken(card, false)
	if token == "" {
		t.Fatal("sign stream token returned empty")
	}
	return &rehomeFixture{handler: handler, sessions: sessions, planner: planner, session: session, token: token, deadURL: deadURL}
}

func (f *rehomeFixture) segment(name string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	f.handler.HandleGetTranscodeSegment(rr, playbackTestRequest(http.MethodGet,
		"/api/v2/playback/transcode/"+f.session.ID+"/segment/"+name+"?st="+f.token, nil,
		map[string]string{"session_id": f.session.ID, "name": name}))
	return rr
}

func pooledNode(id int, url string) *nodepool.Node {
	return &nodepool.Node{ID: id, Name: "node", Type: "transcode", URL: url, Enabled: true, Healthy: true}
}

// TestRelayMovesTranscodeOffUnreachableNodeToAnotherNode covers the reported
// failure: the node died, so the relay's connection is refused. The transcode
// must restart on the healthy node at the requested segment, the request must
// be served from it, and the dead node must stop being selected.
func TestRelayMovesTranscodeOffUnreachableNodeToAnotherNode(t *testing.T) {
	healthy := newFakeRehomeNode(t, false)
	f := newRehomeFixture(t, []*nodepool.Node{
		pooledNode(1, unreachableNodeURL(t)),
		pooledNode(2, healthy.server.URL),
	}, "ffmpeg")

	rr := f.segment("seg_00003.ts")
	if rr.Code != http.StatusOK || rr.Body.String() != "moved:seg_00003.ts" {
		t.Fatalf("status = %d, body = %q; want the segment served by the healthy node", rr.Code, rr.Body.String())
	}

	start := healthy.lastStart()
	if start.SessionID != rehomeTestTransport {
		t.Fatalf("started transport %q, want the session's transport %q", start.SessionID, rehomeTestTransport)
	}
	if start.StartSegmentNumber != 3 || start.SeekSeconds != 6 {
		t.Fatalf("started at segment %d (%.1fs), want segment 3 (6.0s) so numbering continues",
			start.StartSegmentNumber, start.SeekSeconds)
	}
	if start.InputPath != "/media/movie.mkv" || start.TargetCodecVideo != "h264" || !start.RequireReady {
		t.Fatalf("start request lost the recipe: %+v", start)
	}

	session, err := f.sessions.GetSession(f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.TranscodeNodeURL != healthy.server.URL || session.TranscodeTransportID != rehomeTestTransport {
		t.Fatalf("session route = %q/%q, want the healthy node with the same transport", session.TranscodeNodeURL, session.TranscodeTransportID)
	}
	if session.RoutingExecutionNodeID != 2 || session.RoutingEgress != string(noderouting.EgressAPI) {
		t.Fatalf("routing = node %d egress %q, want node 2 behind the API", session.RoutingExecutionNodeID, session.RoutingEgress)
	}
	if f.planner.TranscodeNodeHealthy(f.deadURL) {
		t.Fatal("the unreachable node is still selectable")
	}

	// The next segment goes straight to the new node, with no second start.
	if rr := f.segment("seg_00004.ts"); rr.Code != http.StatusOK || rr.Body.String() != "moved:seg_00004.ts" {
		t.Fatalf("next segment status = %d, body = %q", rr.Code, rr.Body.String())
	}
	if got := healthy.starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want 1", got)
	}
}

// TestRelayMovesTokenlessTranscodeFromStoredRecipe covers a session whose URLs
// carry no stream token: the recipe comes from the node recipe store, and the
// stored card follows the transport to its new node so that node can rebuild
// it after its own restart.
func TestRelayMovesTokenlessTranscodeFromStoredRecipe(t *testing.T) {
	healthy := newFakeRehomeNode(t, false)
	f := newRehomeFixture(t, []*nodepool.Node{
		pooledNode(1, unreachableNodeURL(t)),
		pooledNode(2, healthy.server.URL),
	}, "ffmpeg")
	stored, _ := verifiedStreamCardFromToken(f.token, f.session.ID, f.handler.JWTSecret)
	store := &recordingRecipeCardStoreV3{cards: map[string]playback.RecipeCard{rehomeTestTransport: *stored}}
	f.handler.NodeRecipeStore = store

	rr := httptest.NewRecorder()
	f.handler.HandleGetTranscodeSegment(rr, playbackTestRequest(http.MethodGet,
		"/api/v2/playback/transcode/"+f.session.ID+"/segment/seg_00005.ts", nil,
		map[string]string{"session_id": f.session.ID, "name": "seg_00005.ts"}))
	if rr.Code != http.StatusOK || rr.Body.String() != "moved:seg_00005.ts" {
		t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
	}
	if got := healthy.lastStart().StartSegmentNumber; got != 5 {
		t.Fatalf("started at segment %d, want 5", got)
	}
	if card := store.cards[rehomeTestTransport]; card.TranscodeNodeURL != healthy.server.URL || card.RoutingExecutionNodeID != 2 {
		t.Fatalf("stored recipe names node %q (id %d), want the new node", card.TranscodeNodeURL, card.RoutingExecutionNodeID)
	}
}

// TestRelayMovesTranscodeOffUnreachableNodeToThisServer covers the
// single-node deployment: with no other node, a policy that lets the API
// execute rebuilds the transcode here and serves it locally.
func TestRelayMovesTranscodeOffUnreachableNodeToThisServer(t *testing.T) {
	f := newRehomeFixture(t, []*nodepool.Node{pooledNode(1, unreachableNodeURL(t))}, writePlaybackTestFFmpegSleep(t, "30"))

	rr := httptest.NewRecorder()
	f.handler.HandleGetTranscodeManifest(rr, playbackTestRequest(http.MethodGet,
		"/api/v2/playback/transcode/"+f.session.ID+"/master.m3u8?st="+f.token, nil,
		map[string]string{"session_id": f.session.ID}))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "#EXTM3U") {
		t.Fatalf("status = %d, body = %q; want a manifest served by this server", rr.Code, rr.Body.String())
	}
	if f.handler.tm.GetTranscodeSession(f.session.ID) == nil {
		t.Fatal("no local transcode runs the moved session")
	}
	session, err := f.sessions.GetSession(f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.TranscodeNodeURL != "" || session.RoutingExecution != string(noderouting.ExecutionAPI) || session.RoutingExecutionNodeID != 0 {
		t.Fatalf("session still names node %q (execution %q, node %d)", session.TranscodeNodeURL, session.RoutingExecution, session.RoutingExecutionNodeID)
	}
}

// TestRelayDoesNotMoveTranscodeWhenWorkerOnlyHasNoOtherNode pins that the
// move never crosses a hard execution boundary: worker_only with no healthy
// node left answers the relay failure instead of transcoding on the API.
func TestRelayDoesNotMoveTranscodeWhenWorkerOnlyHasNoOtherNode(t *testing.T) {
	f := newRehomeFixture(t, []*nodepool.Node{pooledNode(1, unreachableNodeURL(t))}, writePlaybackTestFFmpegSleep(t, "30"))
	previous := f.handler.PlaybackConfig
	f.handler.PlaybackConfig = func() config.PlaybackConfig {
		cfg := previous()
		cfg.Routing = config.DefaultPlaybackRoutingPolicy()
		cfg.Routing.VideoTranscodeExecution = config.PlaybackExecutionWorkerOnly
		return cfg
	}

	if rr := f.segment("seg_00003.ts"); rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %q; want 502", rr.Code, rr.Body.String())
	}
	if f.handler.tm.GetTranscodeSession(f.session.ID) != nil {
		t.Fatal("worker_only transcode was moved onto the API")
	}
}

// TestRelayKeepsLiveNodeHTTPErrors pins the other side of the boundary: a node
// that answers is alive, so its HTTP error is relayed and nothing moves.
func TestRelayKeepsLiveNodeHTTPErrors(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "encoder exploded", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	spare := newFakeRehomeNode(t, false)
	f := newRehomeFixture(t, []*nodepool.Node{pooledNode(1, failing.URL), pooledNode(2, spare.server.URL)}, "ffmpeg")

	rr := f.segment("seg_00003.ts")
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "encoder exploded") {
		t.Fatalf("status = %d, body = %q; want the node's own error relayed", rr.Code, rr.Body.String())
	}
	if got := spare.starts.Load(); got != 0 {
		t.Fatalf("starts on the spare node = %d, want 0", got)
	}
	session, err := f.sessions.GetSession(f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.TranscodeNodeURL != failing.URL {
		t.Fatalf("session moved to %q after an HTTP error", session.TranscodeNodeURL)
	}
	if !f.planner.TranscodeNodeHealthy(failing.URL) {
		t.Fatal("a node that answered was marked unhealthy")
	}
}

// TestRelayMovesEachSessionOnceUnderConcurrentRequests pins the pacing: every
// request that finds the node dead waits on one move, so the replacement node
// sees exactly one start however many segment requests arrive together.
func TestRelayMovesEachSessionOnceUnderConcurrentRequests(t *testing.T) {
	healthy := newFakeRehomeNode(t, true)
	f := newRehomeFixture(t, []*nodepool.Node{
		pooledNode(1, unreachableNodeURL(t)),
		pooledNode(2, healthy.server.URL),
	}, "ffmpeg")

	const requests = 8
	results := make(chan *httptest.ResponseRecorder, requests)
	for i := range requests {
		go func() { results <- f.segment("seg_0000" + string(rune('0'+i)) + ".ts") }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for healthy.starts.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no start reached the healthy node")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(healthy.release)
	for range requests {
		rr := <-results
		if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Body.String(), "moved:") {
			t.Fatalf("status = %d, body = %q", rr.Code, rr.Body.String())
		}
	}
	if got := healthy.starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want exactly one move", got)
	}
}
