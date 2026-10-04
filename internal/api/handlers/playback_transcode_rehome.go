package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"

	"github.com/Silo-Server/silo-server/internal/logredact"
	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/noderouting"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/tonemap"
	"github.com/Silo-Server/silo-server/internal/transcodenode"
)

// A transcode whose node dies mid-stream is moved, not failed.
//
// When this server relays a remote transcode (API egress) and the relay cannot
// reach the session's transcode node, the transcode is rebuilt from the same
// recipe on another executor — another pooled transcode node, or this process
// when policy lets the API execute — and the request is served from there. The
// client keeps its URLs: they name the playback session, never the node, and
// the transport identity the node serves under does not change either.
//
// The boundary is deliberate. Only a relay that never got an HTTP response
// counts: a refused or failed connection, or any transport failure while the
// pool already lists the node as unhealthy or no longer has it. A node that
// answers with an HTTP error is alive, and its answer is relayed as before.
// Sessions whose media URLs point at a proxy origin are not relayed here; they
// keep the replan-and-remint rule for failover.

// errTranscodeNodeUnreachable marks a relay that never reached its transcode
// node, so nothing was written to the client.
var errTranscodeNodeUnreachable = errors.New("transcode node unreachable")

var (
	errRehomeRecipeUnavailable = errors.New("no recipe to rebuild the transcode from")
	errRehomeCopyUnsafe        = errors.New("stream-copy recipe is no longer safe to revive")
)

// maxRehomeAttemptsV3 bounds how many executors one move tries. Each failed
// executor is excluded from the next selection, so the bound only matters in a
// large pool where many nodes reject the recipe.
const maxRehomeAttemptsV3 = 8

type transcodeExecutorMoverV3 interface {
	MoveTranscodeExecutor(sessionID string, expected playback.TranscodeRoute, move playback.TranscodeExecutorMove) (bool, error)
}

type transcodeNodeHealthMarkerV3 interface {
	MarkTranscodeNodeUnreachable(nodeURL string) bool
}

type transcodeNodeHealthReaderV3 interface {
	TranscodeNodeHealthy(nodeURL string) bool
}

// transcodeNodeUnreachable reports whether a relay transport error means the
// node is gone rather than slow or mid-response. A connection that could not be
// opened always counts. Any other transport failure counts only when the pool
// already lists the node as unhealthy or no longer pools it.
func (h *PlaybackHandler) transcodeNodeUnreachable(nodeURL string, err error) bool {
	if isConnectFailure(err) {
		return true
	}
	if reader, ok := h.NodePlanner.(transcodeNodeHealthReaderV3); ok {
		return !reader.TranscodeNodeHealthy(nodeURL)
	}
	return false
}

// netOpDial is the net.OpError operation for opening a connection.
const netOpDial = "dial"

// isConnectFailure reports whether err happened while opening the connection,
// before any request byte reached the node.
func isConnectFailure(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == netOpDial {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED)
}

// relayOrRehomeTranscode serves a manifest or segment request for a transcode
// that runs on a transcode node and leaves through this server. suffix is the
// node route after the transport id ("/master.m3u8" or "/segment/<name>").
//
// It returns served=true once a response has been written. Otherwise the
// transcode now runs in this process and the caller serves the returned runtime
// through its ordinary local path.
func (h *PlaybackHandler) relayOrRehomeTranscode(
	w http.ResponseWriter,
	r *http.Request,
	session *playback.Session,
	card *playback.RecipeCard,
	requestedSegment int,
	suffix string,
) (*playback.TranscodeSession, bool) {
	err := h.relayToTranscodeNode(w, r, session.TranscodeNodeURL, "/transcode/"+remoteTransportID(session)+suffix)
	if err == nil {
		return nil, true
	}
	if marker, ok := h.NodePlanner.(transcodeNodeHealthMarkerV3); ok && marker.MarkTranscodeNodeUnreachable(session.TranscodeNodeURL) {
		slog.WarnContext(r.Context(), "transcode node marked unhealthy after the relay could not reach it",
			"component", "api", "node", logredact.SanitizeURL(session.TranscodeNodeURL), "playback_session_id", session.ID)
	}
	dead := playback.TranscodeRoute{NodeURL: session.TranscodeNodeURL, TransportID: session.TranscodeTransportID}
	if card == nil {
		// A live session is served without decoding its token, so the recipe the
		// client carries has not been read yet.
		card, _ = verifiedStreamCardFromToken(r.URL.Query().Get(streamTokenParam), session.ID, h.JWTSecret)
	}
	moveErr := h.tm.RehomeTranscode(r.Context(), session.ID, func(ctx context.Context) error {
		return h.moveTranscodeOffNodeV3(ctx, session.ID, dead, card, requestedSegment)
	})
	if errors.Is(moveErr, errRehomeCopyUnsafe) {
		writePlaybackSessionNotFound(w)
		return nil, true
	}
	if moveErr != nil {
		slog.WarnContext(r.Context(), "could not move the transcode off an unreachable node",
			"component", "api", "node", logredact.SanitizeURL(dead.NodeURL), "playback_session_id", session.ID, "error", moveErr)
		writeTranscodeNodeUnavailable(w)
		return nil, true
	}

	// Serve from wherever the transcode runs now. This request may have joined
	// a move another request led, or found the session already moved.
	if runtime := h.tm.GetTranscodeSession(session.ID); runtime != nil {
		return runtime, false
	}
	current, getErr := h.sessionMgr.GetSession(session.ID)
	if getErr != nil || current.TranscodeNodeURL == "" ||
		(current.TranscodeNodeURL == dead.NodeURL && current.TranscodeTransportID == dead.TransportID) {
		writeTranscodeNodeUnavailable(w)
		return nil, true
	}
	if err := h.relayToTranscodeNode(w, r, current.TranscodeNodeURL, "/transcode/"+remoteTransportID(current)+suffix); err != nil {
		writeTranscodeNodeUnavailable(w)
	}
	return nil, true
}

// moveTranscodeOffNodeV3 rebuilds a session's transcode on a reachable
// executor. It returns nil when the session no longer runs on the dead route,
// whether this call moved it or something else did first.
func (h *PlaybackHandler) moveTranscodeOffNodeV3(
	ctx context.Context,
	sessionID string,
	dead playback.TranscodeRoute,
	tokenCard *playback.RecipeCard,
	requestedSegment int,
) error {
	mover, ok := h.sessionMgr.(transcodeExecutorMoverV3)
	if !ok {
		return errors.New("session manager cannot move a transcode")
	}
	session, err := h.sessionMgr.GetSession(sessionID)
	if err != nil {
		return err
	}
	if session.TranscodeNodeURL != dead.NodeURL || session.TranscodeTransportID != dead.TransportID {
		return nil
	}
	transportID := remoteTransportID(session)
	card, fromStore := h.rehomeRecipeCardV3(ctx, session, transportID, tokenCard)
	if card == nil {
		return errRehomeRecipeUnavailable
	}
	// Moving a stream-copy recipe revives it on a new executor, so it answers
	// to the same persisted copy-safety verdict as any other revival.
	if videoCopyReconstructRefused(ctx, h.fileResolver, h.CopySafetyRacer, card) {
		return errRehomeCopyUnsafe
	}

	workload, delivery := noderouting.WorkloadVideoTranscode, noderouting.DeliveryHLSVideo
	if card.VideoStreamCopy() {
		workload, delivery = noderouting.WorkloadRemux, noderouting.DeliveryHLSRemux
	}
	excludedNodes := map[string]struct{}{nodepool.NormalizeNodeURL(dead.NodeURL): {}}
	excludedShapes := map[string]struct{}{}
	eligible := func(node *nodepool.Node) bool {
		if node == nil {
			return false
		}
		_, skip := excludedNodes[nodepool.NormalizeNodeURL(node.URL)]
		return !skip
	}
	var lastErr error
	for range maxRehomeAttemptsV3 {
		// The client fetches media from this server and keeps doing so, so only
		// API-egress shapes are candidates; the policy still decides between a
		// worker and this process, and a hard execution boundary is never crossed.
		decision, resolveErr := noderouting.Resolve(noderouting.AdaptSessionPlanner(h.NodePlanner), noderouting.ResolveRequest{
			Request: noderouting.Request{
				Workload: workload, Delivery: delivery, Policy: h.playbackRoutingPolicyV3(), ProxyAllowed: false,
			},
			SessionID:            sessionID,
			EstimatedBitrateKbps: card.TargetBitrateKbps,
			TranscodeEligible:    eligible,
			ExcludedShapeIDs:     excludedShapes,
		})
		if resolveErr != nil {
			return resolveErr
		}
		if !decision.Selected() {
			if lastErr != nil {
				return fmt.Errorf("%s: %w", decision.Outcome, lastErr)
			}
			return fmt.Errorf("no executor for the transcode: %s", decision.Outcome)
		}
		if decision.Shape.Execution == noderouting.ExecutionAPI {
			moveErr := h.moveTranscodeToLocalV3(ctx, mover, session, dead, *card, fromStore, requestedSegment)
			if moveErr == nil {
				return nil
			}
			lastErr = moveErr
			excludedShapes[decision.Shape.ID] = struct{}{}
			continue
		}
		node := decision.Plan.TranscodeNode
		moveErr := h.moveTranscodeToNodeV3(ctx, mover, session, dead, *card, fromStore, node, requestedSegment)
		if moveErr == nil {
			return nil
		}
		slog.WarnContext(ctx, "transcode node rejected a moved transcode", "component", "api",
			"node", logredact.SanitizeURL(node.URL), "playback_session_id", sessionID, "error", moveErr)
		if releaser, ok := h.NodePlanner.(sessionReservationReleaserV3); ok {
			releaser.ReleaseSession(sessionID)
		}
		excludedNodes[nodepool.NormalizeNodeURL(node.URL)] = struct{}{}
		lastErr = moveErr
	}
	return fmt.Errorf("every executor refused the transcode: %w", lastErr)
}

// rehomeRecipeCardV3 finds the recipe of the transport being moved: the one the
// client carries in its stream token, or, for a session whose URLs carry no
// token, the one stored for the node to rebuild from. The second result is true
// when the card came from the store, which then has to follow the move.
func (h *PlaybackHandler) rehomeRecipeCardV3(
	ctx context.Context,
	session *playback.Session,
	transportID string,
	tokenCard *playback.RecipeCard,
) (*playback.RecipeCard, bool) {
	if tokenCard != nil && rehomeRecipeServesV3(*tokenCard, session, transportID) {
		card := *tokenCard
		return &card, false
	}
	if h.NodeRecipeStore != nil && h.NodeRecipeStore.Enabled() {
		if stored, ok := h.NodeRecipeStore.Get(ctx, transportID); ok && stored != nil && rehomeRecipeServesV3(*stored, session, transportID) {
			card := *stored
			return &card, true
		}
	}
	return nil, false
}

// rehomeRecipeServesV3 accepts only a complete transcode recipe for exactly the
// transport the session runs now and for the session's owner. A token minted
// for an earlier plan names another transport and is ignored.
func rehomeRecipeServesV3(card playback.RecipeCard, session *playback.Session, transportID string) bool {
	cardTransport := card.TranscodeTransportID
	if cardTransport == "" {
		cardTransport = card.SessionID
	}
	return card.SessionID == session.ID && card.UserID == session.UserID && cardTransport == transportID &&
		card.IsTranscodeRecipe() && playback.ValidateCopyFMP4RecipeCard(card) == nil &&
		card.InputPath != "" && card.SegmentDuration > 0 && card.TargetCodecVideo != ""
}

// moveTranscodeToLocalV3 rebuilds the transport in this process through the
// same reconstruction a restart uses, then points the session at it.
func (h *PlaybackHandler) moveTranscodeToLocalV3(
	ctx context.Context,
	mover transcodeExecutorMoverV3,
	session *playback.Session,
	dead playback.TranscodeRoute,
	card playback.RecipeCard,
	fromStore bool,
	requestedSegment int,
) error {
	local := card
	local.TranscodeNodeURL = ""
	// The dead node's resolved backends describe its hardware, not this host's.
	// Reconstruction resolves them again from the live configuration.
	local.HWAccel, local.EncoderHWAccel, local.HWDevice, local.ToneMapFilter = "", "", "", ""
	if local.RoutingWorkload != "" {
		local.RoutingExecution = string(noderouting.ExecutionAPI)
		local.RoutingExecutionNodeID = 0
	}
	runtime, err := h.tm.ReconstructTranscodeWithError(ctx, session.ID, requestedSegment, local)
	if runtime == nil {
		if err == nil {
			err = errors.New("the transcode could not be rebuilt on this server")
		}
		return err
	}
	moved, err := mover.MoveTranscodeExecutor(session.ID, dead, playback.TranscodeExecutorMove{
		Execution:        string(noderouting.ExecutionAPI),
		TranscodeHWAccel: runtime.Opts().EffectiveEncoderHWAccel(),
	})
	if err != nil || !moved {
		// The session left the dead route while this ran. A local runtime is
		// only stale when the session now runs on a node; a local successor owns
		// the slot otherwise and must be left alone.
		if current, getErr := h.sessionMgr.GetSession(session.ID); getErr != nil || current.TranscodeNodeURL != "" {
			h.tm.CloseTranscodeSessionIf(session.ID, runtime, "")
		}
		return err
	}
	if fromStore {
		// The stored card rebuilt a node transport; a local one never reads it.
		h.deleteNodeRecipeV3(ctx, remoteTransportID(session))
	}
	h.retireUnreachableTransportV3(ctx, dead.NodeURL, remoteTransportID(session))
	slog.WarnContext(ctx, "moved a transcode off an unreachable node", "component", "api",
		"from_node", logredact.SanitizeURL(dead.NodeURL), "executor", "api",
		"playback_session_id", session.ID, "requested_segment", requestedSegment)
	return nil
}

// moveTranscodeToNodeV3 starts the same transport on another transcode node and
// points the session at it once the node has accepted it.
func (h *PlaybackHandler) moveTranscodeToNodeV3(
	ctx context.Context,
	mover transcodeExecutorMoverV3,
	session *playback.Session,
	dead playback.TranscodeRoute,
	card playback.RecipeCard,
	fromStore bool,
	node *nodepool.Node,
	requestedSegment int,
) error {
	transportID := remoteTransportID(session)
	hwAccel := node.EffectiveHWAccel(h.playbackConfig().HWAccel)
	toneMapFilter := ""
	if card.ToneMapMode != "" {
		capabilities, err := h.remoteToneMapCapabilitiesV3(ctx, node.URL, false)
		if err != nil {
			return err
		}
		if !capabilities.Supports(card.ToneMapMode, card.ToneMapSourceKind) {
			return fmt.Errorf("node lacks %s %s tone mapping", card.ToneMapMode, card.ToneMapSourceKind)
		}
		toneMapFilter = capabilities.FilterFor(card.ToneMapMode, card.ToneMapSourceKind)
		hwAccel = playback.HWAccelNone
		if card.ToneMapMode == tonemap.ModeHardware {
			hwAccel = capabilities.BackendFor(card.ToneMapMode, card.ToneMapSourceKind)
		}
	}
	req := rehomeStartRequestV3(card, transportID, hwAccel, requestedSegment)

	var response transcodenode.TranscodeStartResponse
	var status int
	err := h.tm.WithReconstructSlot(ctx, func() error {
		var startErr error
		response, status, startErr = h.startRemotePlaybackTransport(ctx, node.URL, req)
		return startErr
	})
	if err == nil && status != http.StatusAccepted {
		err = fmt.Errorf("transcode node answered start with status %d", status)
	}
	if err == nil {
		err = errors.Join(
			transcodenode.ValidateAudioRecipeAttestation(req, response),
			transcodenode.ValidateCopyFMP4RecipeAttestation(req, response),
			transcodenode.ValidateThrottleAttestation(req, response),
		)
	}
	if err == nil && req.ToneMapMode != "" && response.ToneMapMode != req.ToneMapMode {
		err = errors.New("transcode node did not confirm the tone-map recipe")
	}
	if err != nil {
		// A start that timed out may still have begun; stopping is harmless when
		// it did not.
		h.tm.StopRemoteTranscode(transportID, node.URL)
		return err
	}

	moved, err := mover.MoveTranscodeExecutor(session.ID, dead, playback.TranscodeExecutorMove{
		NodeURL:          node.URL,
		Execution:        string(noderouting.ExecutionTranscode),
		ExecutionNodeID:  node.ID,
		TranscodeHWAccel: firstNonEmptyHandlerV3(strings.TrimSpace(response.EncoderHWAccel), strings.TrimSpace(response.HWAccel), req.HWAccel),
	})
	if err != nil || !moved {
		h.tm.StopRemoteTranscode(transportID, node.URL)
		return err
	}
	if fromStore {
		// The session's URLs carry no recipe, so the node rebuilds from the store
		// after its own restart. The stored card has to name its new executor.
		next := card
		next.TranscodeNodeURL = node.URL
		if next.RoutingWorkload != "" {
			next.RoutingExecutionNodeID = node.ID
		}
		next.HWAccel = firstNonEmptyHandlerV3(strings.TrimSpace(response.HWAccel), req.HWAccel)
		next.EncoderHWAccel = strings.TrimSpace(response.EncoderHWAccel)
		next.SoftwareVideoDecode = req.SoftwareVideoDecode || response.SoftwareVideoDecode
		next.ToneMapFilter = toneMapFilter
		h.putNodeRecipeV3(ctx, transportID, next)
	}
	h.retireUnreachableTransportV3(ctx, dead.NodeURL, transportID)
	slog.WarnContext(ctx, "moved a transcode off an unreachable node", "component", "api",
		"from_node", logredact.SanitizeURL(dead.NodeURL), "executor", "transcode_node",
		"to_node", logredact.SanitizeURL(node.URL), "playback_session_id", session.ID,
		"requested_segment", requestedSegment, "start_segment_number", req.StartSegmentNumber)
	return nil
}

// rehomeStartRequestV3 is the node start request for a moved transport: the
// recipe exactly as recorded, resumed where a reconstruction would resume it.
func rehomeStartRequestV3(card playback.RecipeCard, transportID, hwAccel string, requestedSegment int) transcodenode.TranscodeStartRequest {
	req := transcodenode.TranscodeStartRequest{
		SessionID:                  transportID,
		InputPath:                  card.InputPath,
		SourceVideoCodec:           card.SourceVideoCodec,
		SourceVideoProfile:         card.SourceVideoProfile,
		SourceVideoBitDepth:        card.SourceVideoBitDepth,
		SourceAudioChannels:        card.SourceAudioChannels,
		CopyFMP4RecipeVersion:      card.CopyFMP4RecipeVersion,
		SoftwareVideoDecode:        card.SoftwareVideoDecode,
		ToneMapPolicy:              card.ToneMapPolicy,
		ToneMapMode:                card.ToneMapMode,
		ToneMapSourceKind:          card.ToneMapSourceKind,
		ToneMapRecipeVersion:       card.ToneMapRecipeVersion,
		ToneMapPreflightRequired:   card.ToneMapPreflightRequired,
		ToneMapSourceRevision:      card.ToneMapSourceRevision,
		ToneMapDVConfigPresent:     card.ToneMapDVConfigPresent,
		ToneMapDVBLCompatIDPresent: card.ToneMapDVBLCompatIDPresent,
		ToneMapDVBLPresent:         card.ToneMapDVBLPresent,
		ToneMapDVRPUPresent:        card.ToneMapDVRPUPresent,
		VideoBitstreamFilter:       card.VideoBitstreamFilter,
		VideoSampleEntry:           card.VideoSampleEntry,
		CopyVideoMPEGTS:            card.CopyVideoMPEGTS,
		SeekSeconds:                card.SeekSeconds,
		StreamOriginSeconds:        card.StreamOriginSeconds,
		CopySeekAnchorResolved:     card.CopySeekAnchorResolved,
		StartSegmentNumber:         card.StartSegmentNumber,
		TargetResolution:           card.TargetResolution,
		TargetCodecVideo:           card.TargetCodecVideo,
		TargetCodecAudio:           card.TargetCodecAudio,
		TargetAudioChannels:        card.TargetAudioChannels,
		TargetAudioBitrateKbps:     card.TargetAudioBitrateKbps,
		TargetBitrateKbps:          card.TargetBitrateKbps,
		SegmentDuration:            card.SegmentDuration,
		HWAccel:                    hwAccel,
		AudioTrackIndex:            card.AudioTrackIndex,
		SubtitleTrackIndex:         card.SubtitleTrackIndex,
		SubtitleBurnIn:             card.SubtitleBurnIn,
		SubtitleCodec:              card.SubtitleCodec,
		TotalDuration:              card.TotalDuration,
		ThrottleSeconds:            card.ThrottleSeconds,
		RequireReady:               true,
	}
	if segment, seekSeconds, ok := playback.ReconstructResumePoint(card, requestedSegment); ok {
		req.StartSegmentNumber = segment
		req.SeekSeconds = seekSeconds
	}
	if playback.IsAudioToAACStereoDownmixV3(req.SourceAudioChannels, req.TargetCodecAudio, req.TargetAudioChannels) {
		req.TargetAudioChannels = 2
		req.AudioRecipeVersion = playback.TransformationAudioToAACRecipeVersionV3
	} else {
		// SourceAudioChannels is a v2 recipe field at the node boundary; an
		// ordinary encode must not carry it, exactly as at a fresh start.
		req.SourceAudioChannels = 0
	}
	return req
}

// retireUnreachableTransportV3 asks the node the transport left to drop it, in
// case the node is only unreachable from here and still running FFmpeg. A dead
// node refuses the connection at once; the answer is not waited on.
func (h *PlaybackHandler) retireUnreachableTransportV3(ctx context.Context, nodeURL, transportID string) {
	detached := context.WithoutCancel(ctx)
	go func() {
		if err := h.tm.CancelRemoteTranscode(detached, transportID, nodeURL); err != nil {
			slog.DebugContext(detached, "could not stop the transcode on the node it moved off",
				"component", "api", "node", logredact.SanitizeURL(nodeURL), "transport", transportID, "error", err)
		}
	}()
}
