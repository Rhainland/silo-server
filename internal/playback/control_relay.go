package playback

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Control relay: run a playback control request on the API replica that holds
// the session's realtime lane.
//
// A session's control lane (RealtimeHub), the tracker that enforces a
// command's deadline and the administrator command ledger live in the process
// that accepted the player's control socket. Behind a load balancer an
// administrator request can land on any replica, which finds no lane and,
// usually, no session either.
//
// The relay records lane ownership in Redis: one key per session naming the
// replica holding the lane. A replica claims the key when a lane opens or a
// new connection takes it over (the newest connection wins), refreshes it
// while the lane stays open, and deletes it when the lane closes if it still
// names this replica. A replica without the lane looks the owner up and
// publishes the request on cache.ChannelPlaybackControl addressed to it. The
// owner checks that it still holds the lane, runs the request through its
// local handler, and publishes the reply addressed back. Every request for a
// session therefore executes in the one process that holds its lane, so the
// command ledger's applied-once ordering holds across the hop.
//
// Failure model. Forward reports ok=false, and the caller serves the request
// locally exactly as a single replica would, when this replica holds the lane,
// when no other replica claims it, when Redis or the bus fails, when the owner
// answers that it no longer holds the lane, or when no reply arrives within
// controlRelayReplyTimeout. An owner that died leaves its claim until the TTL
// lapses or the player reconnects to a surviving replica, which claims the
// key afresh; until then a request waits out the reply timeout and then falls
// back. A reply lost after the owner executed is indistinguishable from a
// dead owner: the caller falls back, and a retry with the same command
// identity reaches the owner again and replays its receipt.
//
// Without Redis there is no relay (NewControlRelay returns nil) and every
// request is served locally, which is complete on a single replica.

const (
	controlRelayLaneKeyPrefix = "silo:playback:lane:"

	// controlRelayLaneTTL bounds how long a dead replica's claim survives.
	controlRelayLaneTTL = 30 * time.Second
	// controlRelayRefreshInterval re-asserts the claims of open lanes well
	// inside the TTL.
	controlRelayRefreshInterval = 10 * time.Second
	// controlRelayReplyTimeout bounds the wait for the owner's reply. The
	// owner's work is one bounded lane write (5 s socket write deadline) plus,
	// for a terminate, the session stop; the margin covers the bus.
	controlRelayReplyTimeout = 8 * time.Second
	// controlRelayRedisTimeout bounds one ownership read or write.
	controlRelayRedisTimeout = time.Second
	// controlRelayQueueSize bounds pending lane open/close notifications; an
	// overflow is repaired by the next refresh.
	controlRelayQueueSize = 256

	controlRelayKindRequest = "request"
	controlRelayKindReply   = "reply"
)

// ControlRelayHandler executes a relayed request on the owning replica and
// returns the reply body. It must not forward again.
type ControlRelayHandler func(ctx context.Context, sessionID, op string, body json.RawMessage) json.RawMessage

// ControlRelayOwners records which replica holds a session's lane. The
// production store is Redis (NewControlRelay); tests supply their own.
type ControlRelayOwners interface {
	// Claim names owner unconditionally: the newest connection wins.
	Claim(ctx context.Context, sessionID, owner string) error
	// Refresh extends owner's claim, or claims a key nobody holds, but never
	// takes a key another replica claimed later.
	Refresh(ctx context.Context, sessionID, owner string) error
	// Release deletes the key if it still names owner.
	Release(ctx context.Context, sessionID, owner string) error
	// Owner returns the replica holding the lane, or "" when none does.
	Owner(ctx context.Context, sessionID string) (string, error)
}

type controlRelayMessage struct {
	Kind      string          `json:"kind"`
	RequestID string          `json:"request_id"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	SessionID string          `json:"session_id"`
	Op        string          `json:"op,omitempty"`
	Handled   bool            `json:"handled,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
}

type controlRelayLaneEvent struct {
	sessionID string
	open      bool
}

// ControlRelay routes playback control requests to the lane owner. A nil
// *ControlRelay never forwards.
type ControlRelay struct {
	id     string
	hub    *RealtimeHub
	owners ControlRelayOwners

	replyTimeout    time.Duration
	refreshInterval time.Duration

	mu      sync.Mutex
	handler ControlRelayHandler
	publish func(context.Context, string) error
	pending map[string]chan controlRelayMessage
	events  chan controlRelayLaneEvent
	started bool
}

// NewControlRelay returns a relay for hub backed by client, or nil without
// Redis.
func NewControlRelay(hub *RealtimeHub, client *redis.Client) *ControlRelay {
	if hub == nil || client == nil {
		return nil
	}
	return NewControlRelayWithOwners(hub, &redisControlRelayOwners{client: client, ttl: controlRelayLaneTTL})
}

// NewControlRelayWithOwners returns a relay for hub that records lane
// ownership in owners.
func NewControlRelayWithOwners(hub *RealtimeHub, owners ControlRelayOwners) *ControlRelay {
	if hub == nil || owners == nil {
		return nil
	}
	return &ControlRelay{
		id:              uuid.NewString(),
		hub:             hub,
		owners:          owners,
		replyTimeout:    controlRelayReplyTimeout,
		refreshInterval: controlRelayRefreshInterval,
		pending:         make(map[string]chan controlRelayMessage),
		events:          make(chan controlRelayLaneEvent, controlRelayQueueSize),
	}
}

// Handle installs the handler that executes requests relayed to this replica.
func (r *ControlRelay) Handle(handler ControlRelayHandler) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.handler = handler
	r.mu.Unlock()
}

// Start subscribes to the control channel, starts tracking this replica's
// lanes and claims those already open. Call it once with the server lifetime
// context; publish and subscribe carry one payload on
// cache.ChannelPlaybackControl. On ctx's end the replica releases its claims.
func (r *ControlRelay) Start(
	ctx context.Context,
	publish func(context.Context, string) error,
	subscribe func(context.Context, func(string)) error,
) error {
	if r == nil || publish == nil || subscribe == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return nil
	}
	if err := subscribe(ctx, r.receive); err != nil {
		return err
	}
	r.publish = publish
	r.started = true
	r.hub.SetLaneObserver(r.observeLane)
	go r.run(ctx)
	return nil
}

// Forward runs op for sessionID on the replica holding the session's lane and
// returns its reply body. ok is false when the caller should serve the
// request itself (see the failure model above).
func (r *ControlRelay) Forward(ctx context.Context, sessionID, op string, body any) (reply json.RawMessage, ok bool) {
	if r == nil || sessionID == "" || r.hub.HasLane(sessionID) {
		return nil, false
	}
	r.mu.Lock()
	publish := r.publish
	r.mu.Unlock()
	if publish == nil {
		return nil, false
	}

	lookupCtx, cancel := context.WithTimeout(ctx, controlRelayRedisTimeout)
	owner, err := r.owners.Owner(lookupCtx, sessionID)
	cancel()
	if err != nil {
		slog.WarnContext(ctx, "playback control relay: owner lookup failed", "component", "playback", "playback_session_id", sessionID, "error", err)
		return nil, false
	}
	if owner == "" || owner == r.id {
		return nil, false
	}

	encodedBody, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	request := controlRelayMessage{
		Kind:      controlRelayKindRequest,
		RequestID: uuid.NewString(),
		From:      r.id,
		To:        owner,
		SessionID: sessionID,
		Op:        op,
		Body:      encodedBody,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, false
	}

	replies := make(chan controlRelayMessage, 1)
	r.mu.Lock()
	r.pending[request.RequestID] = replies
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, request.RequestID)
		r.mu.Unlock()
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, r.replyTimeout)
	defer cancelWait()
	if err := publish(waitCtx, string(payload)); err != nil {
		slog.WarnContext(ctx, "playback control relay: publish failed", "component", "playback", "playback_session_id", sessionID, "op", op, "error", err)
		return nil, false
	}
	select {
	case msg := <-replies:
		if !msg.Handled {
			return nil, false
		}
		return msg.Body, true
	case <-waitCtx.Done():
		slog.WarnContext(ctx, "playback control relay: owning replica did not answer; serving locally", "component", "playback", "playback_session_id", sessionID, "op", op)
		return nil, false
	}
}

// receive runs on the bus reader: it hands replies to their waiter and
// executes requests off the reader so a slow lane cannot stall the bus.
func (r *ControlRelay) receive(payload string) {
	var msg controlRelayMessage
	if err := json.Unmarshal([]byte(payload), &msg); err != nil || msg.To != r.id || msg.RequestID == "" {
		return
	}
	switch msg.Kind {
	case controlRelayKindReply:
		r.mu.Lock()
		waiter := r.pending[msg.RequestID]
		r.mu.Unlock()
		if waiter != nil {
			select {
			case waiter <- msg:
			default:
			}
		}
	case controlRelayKindRequest:
		go r.serve(msg)
	}
}

func (r *ControlRelay) serve(request controlRelayMessage) {
	r.mu.Lock()
	handler := r.handler
	publish := r.publish
	r.mu.Unlock()
	if publish == nil {
		return
	}
	reply := controlRelayMessage{
		Kind:      controlRelayKindReply,
		RequestID: request.RequestID,
		From:      r.id,
		To:        request.From,
		SessionID: request.SessionID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.replyTimeout)
	defer cancel()
	// Only the lane holder executes. A claim that outlived its lane answers
	// at once so the caller falls back without waiting out the timeout.
	if handler != nil && r.hub.HasLane(request.SessionID) {
		reply.Body = handler(ctx, request.SessionID, request.Op, request.Body)
		reply.Handled = true
	}
	payload, err := json.Marshal(reply)
	if err != nil {
		return
	}
	if err := publish(ctx, string(payload)); err != nil {
		slog.WarnContext(ctx, "playback control relay: reply publish failed", "component", "playback", "playback_session_id", request.SessionID, "op", request.Op, "error", err)
	}
}

// observeLane runs under the hub's Register/Unregister; it only enqueues.
func (r *ControlRelay) observeLane(sessionID string, open bool) {
	select {
	case r.events <- controlRelayLaneEvent{sessionID: sessionID, open: open}:
	default:
		// The refresh tick claims open lanes; a missed release expires.
	}
}

// run applies lane changes in order and refreshes claims until ctx ends.
func (r *ControlRelay) run(ctx context.Context) {
	ticker := time.NewTicker(r.refreshInterval)
	defer ticker.Stop()
	r.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			r.releaseAll()
			return
		case event := <-r.events:
			r.applyLaneEvent(ctx, event)
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

// applyLaneEvent acts on the lane's current state rather than the event, so
// notifications that raced each other cannot leave a claim for a closed lane
// or drop the claim of an open one.
func (r *ControlRelay) applyLaneEvent(ctx context.Context, event controlRelayLaneEvent) {
	opCtx, cancel := context.WithTimeout(ctx, controlRelayRedisTimeout)
	defer cancel()
	var err error
	switch open := r.hub.HasLane(event.sessionID); {
	case open && event.open:
		err = r.owners.Claim(opCtx, event.sessionID, r.id)
	case open:
		err = r.owners.Refresh(opCtx, event.sessionID, r.id)
	default:
		err = r.owners.Release(opCtx, event.sessionID, r.id)
	}
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "playback control relay: lane ownership update failed", "component", "playback", "playback_session_id", event.sessionID, "error", err)
	}
}

func (r *ControlRelay) refresh(ctx context.Context) {
	for _, sessionID := range r.hub.LaneSessionIDs() {
		if ctx.Err() != nil {
			return
		}
		opCtx, cancel := context.WithTimeout(ctx, controlRelayRedisTimeout)
		err := r.owners.Refresh(opCtx, sessionID, r.id)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "playback control relay: lane claim refresh failed", "component", "playback", "playback_session_id", sessionID, "error", err)
		}
	}
}

// releaseAll gives up this replica's claims on shutdown so requests stop
// waiting for it at once instead of after the TTL.
func (r *ControlRelay) releaseAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, sessionID := range r.hub.LaneSessionIDs() {
		if ctx.Err() != nil {
			return
		}
		_ = r.owners.Release(ctx, sessionID, r.id)
	}
}

// redisControlRelayOwners stores lane ownership as one expiring key per
// session.
type redisControlRelayOwners struct {
	client *redis.Client
	ttl    time.Duration
}

var (
	// controlRelayRefreshScript extends the caller's claim or claims a free
	// key, but leaves a key another replica claimed later alone.
	controlRelayRefreshScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current == false or current == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
return 0`)
	// controlRelayReleaseScript deletes the key only if it names the caller.
	controlRelayReleaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)
)

func controlRelayLaneKey(sessionID string) string {
	return controlRelayLaneKeyPrefix + sessionID
}

func (o *redisControlRelayOwners) Claim(ctx context.Context, sessionID, owner string) error {
	return o.client.Set(ctx, controlRelayLaneKey(sessionID), owner, o.ttl).Err()
}

func (o *redisControlRelayOwners) Refresh(ctx context.Context, sessionID, owner string) error {
	return controlRelayRefreshScript.Run(ctx, o.client, []string{controlRelayLaneKey(sessionID)}, owner, o.ttl.Milliseconds()).Err()
}

func (o *redisControlRelayOwners) Release(ctx context.Context, sessionID, owner string) error {
	return controlRelayReleaseScript.Run(ctx, o.client, []string{controlRelayLaneKey(sessionID)}, owner).Err()
}

func (o *redisControlRelayOwners) Owner(ctx context.Context, sessionID string) (string, error) {
	owner, err := o.client.Get(ctx, controlRelayLaneKey(sessionID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return owner, err
}
