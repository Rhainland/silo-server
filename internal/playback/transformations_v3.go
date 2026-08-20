package playback

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type transformationRegistryCacheKeyV3 struct {
	ffmpegPath string
	hwAccel    string
	modTimeNS  int64
	size       int64
}

type transformationRegistryCacheEntryV3 struct {
	registry  *TransformationRegistryV3
	expiresAt time.Time
}

const transformationRegistryIncompleteTTL = 5 * time.Second

var transformationRegistryCacheV3 = struct {
	sync.Mutex
	values map[transformationRegistryCacheKeyV3]transformationRegistryCacheEntryV3
}{values: make(map[transformationRegistryCacheKeyV3]transformationRegistryCacheEntryV3)}

var transformationRegistryProbeGroupV3 singleflight.Group

// ValidateRequiredTransformationsV3 verifies that every required server recipe
// is present at the exact version in an executor's current advertisement.
func ValidateRequiredTransformationsV3(required, advertised []TransformationV3) error {
	available := make(map[string]string, len(advertised))
	for _, transformation := range advertised {
		available[strings.ToLower(strings.TrimSpace(transformation.Name))] = strings.TrimSpace(transformation.RecipeVersion)
	}
	for _, transformation := range required {
		if strings.EqualFold(strings.TrimSpace(transformation.Executor), ExecutorClientV3) {
			continue
		}
		version, ok := available[strings.ToLower(strings.TrimSpace(transformation.Name))]
		if !ok || version != strings.TrimSpace(transformation.RecipeVersion) {
			return fmt.Errorf("executor lacks transformation %s@%s", transformation.Name, transformation.RecipeVersion)
		}
	}
	return nil
}

// ValidateRequiredTransformationsForExecutionV3 rejects a recipe only when
// the executor probe completed and authoritatively reported it unavailable. A
// transiently incomplete probe must not turn an otherwise executable recipe
// into a request-path outage; FFmpeg remains the final execution check.
func ValidateRequiredTransformationsForExecutionV3(required []TransformationV3, registry *TransformationRegistryV3) error {
	if registry != nil && !registry.ProbeObserved() {
		return nil
	}
	return ValidateRequiredTransformationsV3(required, registry.Advertised())
}

// FFmpeg filter names the H.264 ladder needs per executor. These are probe
// tokens matched against `ffmpeg -filters`; transcode.go builds the actual
// filter graphs that use them.
const (
	filterHWUpload     = "hwupload"
	filterHWUploadCUDA = "hwupload_cuda"
	filterHWMap        = "hwmap"
	filterScaleVAAPI   = "scale_vaapi"
	filterScaleCUDA    = "scale_cuda"
)

type TransformationSpecV3 struct {
	Name                 string
	RecipeVersion        string
	Available            bool
	RequiredCapability   string
	PromisedDynamicRange string
	ValidatedClaims      []string
	TerminalReason       string
}

type TransformationRegistryV3 struct {
	entries       map[string]TransformationSpecV3
	probeObserved bool
}

func ProbeTransformationRegistryV3(ctx context.Context, ffmpegPath string) *TransformationRegistryV3 {
	return ProbeTransformationRegistryForExecutorV3(ctx, ffmpegPath, HWAccelNone)
}

// DetectExecutorCapabilitiesV3 keeps hardware and transformation discovery in
// one place so proxy and transcode-node capability responses cannot drift.
func DetectExecutorCapabilitiesV3(ctx context.Context, ffmpegPath, hwAccel string) HWAccelInfo {
	info := DetectHWAccelWithFFmpeg(ffmpegPath)
	info.Transformations = ProbeTransformationRegistryForExecutorV3(ctx, ffmpegPath, hwAccel).Advertised()
	return info
}

// ProbeTransformationRegistryForExecutorV3 reports recipes executable by the
// encoder selected from this executor's live HWAccel configuration. Merely
// having some unrelated H.264 encoder in the binary is insufficient.
func ProbeTransformationRegistryForExecutorV3(ctx context.Context, ffmpegPath, hwAccel string) *TransformationRegistryV3 {
	// Resolve exactly like the execution paths (remux and transcode) so every
	// capability advertised here holds for the binary that later runs.
	ffmpegPath = ResolveFFmpegPath(ffmpegPath)
	resolvedHWAccel := ResolveHWAccelWithFFmpeg(hwAccel, ffmpegPath)
	key := transformationRegistryCacheKeyV3{ffmpegPath: ffmpegPath, hwAccel: resolvedHWAccel}
	if info, err := os.Stat(ffmpegPath); err == nil {
		key.modTimeNS = info.ModTime().UnixNano()
		key.size = info.Size()
	}
	if cached := cachedTransformationRegistryV3(key, time.Now()); cached != nil {
		return cached
	}
	flightKey := fmt.Sprintf("%s\x00%s\x00%d\x00%d", key.ffmpegPath, key.hwAccel, key.modTimeNS, key.size)
	resultCh := transformationRegistryProbeGroupV3.DoChan(flightKey, func() (any, error) {
		if cached := cachedTransformationRegistryV3(key, time.Now()); cached != nil {
			return cached, nil
		}
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		registry := probeTransformationRegistryForExecutorV3(probeCtx, ffmpegPath, resolvedHWAccel)
		entry := transformationRegistryCacheEntryV3{registry: registry}
		if !registry.ProbeObserved() {
			entry.expiresAt = time.Now().Add(transformationRegistryIncompleteTTL)
		}
		transformationRegistryCacheV3.Lock()
		transformationRegistryCacheV3.values[key] = entry
		transformationRegistryCacheV3.Unlock()
		return registry, nil
	})
	select {
	case <-ctx.Done():
		return newTransformationRegistryV3(nil, false)
	case result := <-resultCh:
		if result.Err != nil {
			return newTransformationRegistryV3(nil, false)
		}
		registry, ok := result.Val.(*TransformationRegistryV3)
		if !ok || registry == nil {
			return newTransformationRegistryV3(nil, false)
		}
		return registry
	}
}

func cachedTransformationRegistryV3(key transformationRegistryCacheKeyV3, now time.Time) *TransformationRegistryV3 {
	transformationRegistryCacheV3.Lock()
	defer transformationRegistryCacheV3.Unlock()
	entry, ok := transformationRegistryCacheV3.values[key]
	if !ok {
		return nil
	}
	if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
		delete(transformationRegistryCacheV3.values, key)
		return nil
	}
	return entry.registry
}

func probeTransformationRegistryForExecutorV3(ctx context.Context, ffmpegPath, resolvedHWAccel string) *TransformationRegistryV3 {
	bsfCtx, cancelBSF := context.WithTimeout(ctx, 3*time.Second)
	bsfs, bsfErr := exec.CommandContext(bsfCtx, ffmpegPath, "-hide_banner", "-bsfs").Output()
	cancelBSF()
	encoderCtx, cancelEncoders := context.WithTimeout(ctx, 3*time.Second)
	encoders, encoderErr := exec.CommandContext(encoderCtx, ffmpegPath, "-hide_banner", "-encoders").Output()
	cancelEncoders()
	filterCtx, cancelFilters := context.WithTimeout(ctx, 3*time.Second)
	filters, filterErr := exec.CommandContext(filterCtx, ffmpegPath, "-hide_banner", "-filters").Output()
	cancelFilters()
	_, ffmpegErr := exec.LookPath(ffmpegPath)
	probesObserved := bsfErr == nil && encoderErr == nil && filterErr == nil
	registry := newTransformationRegistryV3([]TransformationSpecV3{
		{Name: TransformationServerDV7HDR10V3, RecipeVersion: "1", Available: ffmpegOutputHasToken(bsfs, "dovi_rpu"), RequiredCapability: "ffmpeg_bsf:dovi_rpu", PromisedDynamicRange: DynamicRangeHDR10V3, ValidatedClaims: DV7ToHDR10ClaimsV3(), TerminalReason: TerminalDVConversionUnsupportedV3},
		{Name: TransformationAudioToAACV3, RecipeVersion: "1", Available: ffmpegErr == nil && ffmpegOutputHasToken(encoders, "aac"), RequiredCapability: "ffmpeg_encoder:aac", ValidatedClaims: []string{ClaimAudioDecodeV3}, TerminalReason: TerminalAudioConversionUnsupportedV3},
		{Name: TransformationVideoToH264V3, RecipeVersion: TransformationVideoToH264RecipeVersionV3, Available: ffmpegErr == nil && h264EncoderAvailableForExecutorV3(encoders, resolvedHWAccel) && h264FiltersAvailableForExecutorV3(filters, resolvedHWAccel), RequiredCapability: "ffmpeg_encoder_and_filters:h264", PromisedDynamicRange: DynamicRangeSDRV3, ValidatedClaims: []string{ClaimH264DecodeV3}, TerminalReason: TerminalVideoConversionUnsupportedV3},
	}, probesObserved)
	if !probesObserved {
		slog.WarnContext(ctx, "ffmpeg transformation probe incomplete; caching briefly",
			"component", "playback", "ffmpeg", ffmpegPath,
			"bsf_error", bsfErr, "encoder_error", encoderErr, "filter_error", filterErr)
	}
	return registry
}

func h264FiltersAvailableForExecutorV3(filters []byte, resolvedHWAccel string) bool {
	required := []string(nil)
	switch strings.ToLower(strings.TrimSpace(resolvedHWAccel)) {
	case transcodeHWQSV:
		required = []string{filterHWUpload, filterScaleVAAPI, filterHWMap}
	case transcodeHWVAAPI:
		required = []string{filterHWUpload, filterScaleVAAPI}
	case transcodeHWNVENC:
		required = []string{filterHWUploadCUDA, filterScaleCUDA}
	}
	for _, name := range required {
		if !ffmpegOutputHasToken(filters, name) {
			return false
		}
	}
	return true
}

// h264EncodersV3 lists every H.264 encoder the transcode pipeline can select
// (see buildTranscodeArgs' hardware ladder in transcode.go); any one of them
// satisfies the video_to_h264 transformation.
func h264EncoderAvailableForExecutorV3(encoders []byte, resolvedHWAccel string) bool {
	encoder := encoderH264Software
	switch strings.ToLower(strings.TrimSpace(resolvedHWAccel)) {
	case transcodeHWQSV:
		encoder = encoderH264QSV
	case transcodeHWVAAPI:
		encoder = encoderH264VAAPI
	case transcodeHWNVENC:
		encoder = encoderH264NVENC
	}
	// MPEG-4 Part 2 input is intentionally decoded and encoded in software even
	// on hardware-configured executors. Because the registry is source-agnostic,
	// advertising recipe v3 must cover both the configured encoder and that
	// mandatory libx264 fallback.
	return ffmpegOutputHasToken(encoders, encoder) && ffmpegOutputHasToken(encoders, encoderH264Software)
}

func NewTransformationRegistryV3(specs []TransformationSpecV3) *TransformationRegistryV3 {
	return newTransformationRegistryV3(specs, true)
}

func newTransformationRegistryV3(specs []TransformationSpecV3, probeObserved bool) *TransformationRegistryV3 {
	r := &TransformationRegistryV3{entries: make(map[string]TransformationSpecV3, len(specs)), probeObserved: probeObserved}
	for _, spec := range specs {
		if spec.Name != "" {
			r.entries[spec.Name] = spec
		}
	}
	return r
}

func (r *TransformationRegistryV3) ProbeObserved() bool {
	return r == nil || r.probeObserved
}

func (r *TransformationRegistryV3) Available(name string) bool {
	if r == nil {
		return false
	}
	spec, ok := r.entries[name]
	return ok && spec.Available
}

// WithAdvertised returns a registry whose known specs are additionally marked
// available when a pooled transcode node advertises the same server-executed
// transformation at the same recipe version. Advertisements never introduce
// new specs: the planner only selects transformations this server defines,
// and pinning versions to the local spec guarantees a plan built from the
// widened registry passes the per-node advertisement validation at transport
// time. Returns the receiver unchanged when nothing new becomes available.
func (r *TransformationRegistryV3) WithAdvertised(advertised []TransformationV3) *TransformationRegistryV3 {
	if r == nil || len(advertised) == 0 {
		return r
	}
	specs := make([]TransformationSpecV3, 0, len(r.entries))
	changed := false
	for _, spec := range r.entries {
		if !spec.Available {
			for _, remote := range advertised {
				if strings.EqualFold(strings.TrimSpace(remote.Name), spec.Name) &&
					strings.TrimSpace(remote.RecipeVersion) == spec.RecipeVersion &&
					strings.EqualFold(strings.TrimSpace(remote.Executor), "server") {
					spec.Available = true
					changed = true
					break
				}
			}
		}
		specs = append(specs, spec)
	}
	if !changed {
		return r
	}
	return newTransformationRegistryV3(specs, r.probeObserved)
}

func (r *TransformationRegistryV3) Advertised() []TransformationV3 {
	if r == nil || !r.probeObserved {
		return nil
	}
	result := make([]TransformationV3, 0, len(r.entries))
	for _, spec := range r.entries {
		if spec.Available {
			result = append(result, TransformationV3{Name: spec.Name, Executor: ExecutorServerV3, RecipeVersion: spec.RecipeVersion, ValidatedClaims: append([]string(nil), spec.ValidatedClaims...)})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
