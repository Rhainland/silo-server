package handlers

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Silo-Server/silo-server/internal/playback"
)

// Cross-replica administrator commands.
//
// A sequenced command or a terminate must run where the session's control
// lane is: the lane delivers the command, the tracker on that replica sees
// the player's result and runs the deadline fallback, and the ledger there
// orders every command for the session. Command and Terminate ask the
// playback.ControlRelay first; when another replica holds the lane the relay
// runs the request there through ServeRelayed and returns its outcome, which
// is decoded back into the same view or error the local path produces. When
// the relay reports no reachable owner the request is served here, which is
// the single-replica behavior.

const (
	adminRelayOpCommand   = "admin_playback_command"
	adminRelayOpTerminate = "admin_playback_terminate"

	adminRelayErrNotFound         = "session_not_found"
	adminRelayErrStale            = "stale"
	adminRelayErrConflict         = "conflict"
	adminRelayErrRealtimeRequired = "realtime_required"
	adminRelayErrInvalid          = "invalid"
	adminRelayErrUnavailable      = "unavailable"
	adminRelayErrInternal         = "internal"
)

// errAdminRelayRemote is an unexpected failure on the owning replica; it
// maps to 500 like any unclassified local error.
var errAdminRelayRemote = errors.New("playback control failed on the replica holding the session")

type adminRelayCommandReply struct {
	View   AdminPlaybackCommandView `json:"view"`
	Error  string                   `json:"error,omitempty"`
	Latest int64                    `json:"latest,omitempty"`
}

type adminRelayTerminateReply struct {
	View  AdminTerminateView `json:"view"`
	Error string             `json:"error,omitempty"`
}

// UseControlRelay routes commands and terminates for sessions whose control
// lane is on another replica through relay, and serves the requests relayed
// to this replica.
func (h *AdminPlaybackControlHandler) UseControlRelay(relay *playback.ControlRelay) {
	if h == nil || relay == nil {
		return
	}
	relay.Handle(h.ServeRelayed)
	h.relay = relay
}

// ServeRelayed executes a request relayed from another replica on this one.
func (h *AdminPlaybackControlHandler) ServeRelayed(ctx context.Context, _ string, op string, body json.RawMessage) json.RawMessage {
	var reply any
	switch op {
	case adminRelayOpCommand:
		var in AdminPlaybackCommandInput
		if err := json.Unmarshal(body, &in); err != nil {
			reply = adminRelayCommandReply{Error: adminRelayErrInvalid}
			break
		}
		view, err := h.commandLocal(ctx, in)
		out := adminRelayCommandReply{View: view}
		out.Error, out.Latest = encodeAdminRelayError(err)
		reply = out
	case adminRelayOpTerminate:
		var in AdminTerminateInput
		if err := json.Unmarshal(body, &in); err != nil {
			reply = adminRelayTerminateReply{Error: adminRelayErrInvalid}
			break
		}
		view, err := h.terminateLocal(ctx, in)
		out := adminRelayTerminateReply{View: view}
		out.Error, _ = encodeAdminRelayError(err)
		reply = out
	default:
		reply = adminRelayCommandReply{Error: adminRelayErrInvalid}
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		return nil
	}
	return encoded
}

// relayCommand runs the command on the lane owner. ok is false when it
// should run here.
func (h *AdminPlaybackControlHandler) relayCommand(ctx context.Context, in AdminPlaybackCommandInput) (view AdminPlaybackCommandView, relayed bool, err error) {
	body, ok := h.relay.Forward(ctx, in.SessionID, adminRelayOpCommand, in)
	if !ok {
		return AdminPlaybackCommandView{}, false, nil
	}
	var reply adminRelayCommandReply
	if json.Unmarshal(body, &reply) != nil {
		return AdminPlaybackCommandView{}, true, errAdminRelayRemote
	}
	if reply.Error != "" {
		return AdminPlaybackCommandView{}, true, decodeAdminRelayError(reply.Error, reply.Latest, ErrAdminPlaybackCommandUnavailable)
	}
	return reply.View, true, nil
}

// relayTerminate runs the terminate on the lane owner. ok is false when it
// should run here.
func (h *AdminPlaybackControlHandler) relayTerminate(ctx context.Context, in AdminTerminateInput) (view AdminTerminateView, relayed bool, err error) {
	body, ok := h.relay.Forward(ctx, in.SessionID, adminRelayOpTerminate, in)
	if !ok {
		return AdminTerminateView{}, false, nil
	}
	var reply adminRelayTerminateReply
	if json.Unmarshal(body, &reply) != nil {
		return AdminTerminateView{}, true, errAdminRelayRemote
	}
	if reply.Error != "" {
		return AdminTerminateView{}, true, decodeAdminRelayError(reply.Error, 0, ErrAdminTerminateUnavailable)
	}
	return reply.View, true, nil
}

func encodeAdminRelayError(err error) (code string, latest int64) {
	var stale *AdminPlaybackCommandStaleError
	switch {
	case err == nil:
		return "", 0
	case errors.As(err, &stale):
		return adminRelayErrStale, stale.Latest
	case errors.Is(err, playback.ErrSessionNotFound):
		return adminRelayErrNotFound, 0
	case errors.Is(err, ErrAdminPlaybackCommandConflict):
		return adminRelayErrConflict, 0
	case errors.Is(err, ErrAdminPlaybackRealtimeRequired):
		return adminRelayErrRealtimeRequired, 0
	case errors.Is(err, ErrAdminPlaybackCommandInvalid):
		return adminRelayErrInvalid, 0
	case errors.Is(err, ErrAdminPlaybackCommandUnavailable), errors.Is(err, ErrAdminTerminateUnavailable):
		return adminRelayErrUnavailable, 0
	default:
		return adminRelayErrInternal, 0
	}
}

// decodeAdminRelayError restores the error the owner reported; unavailable
// is the operation's own unavailability error.
func decodeAdminRelayError(code string, latest int64, unavailable error) error {
	switch code {
	case adminRelayErrStale:
		return &AdminPlaybackCommandStaleError{Latest: latest}
	case adminRelayErrNotFound:
		return playback.ErrSessionNotFound
	case adminRelayErrConflict:
		return ErrAdminPlaybackCommandConflict
	case adminRelayErrRealtimeRequired:
		return ErrAdminPlaybackRealtimeRequired
	case adminRelayErrInvalid:
		return ErrAdminPlaybackCommandInvalid
	case adminRelayErrUnavailable:
		return unavailable
	default:
		return errAdminRelayRemote
	}
}
