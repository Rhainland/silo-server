package playback

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type transformationRegistryCacheKeyV3 struct {
	ffmpegPath string
	hwAccel    string
	modTimeNS  int64
	size       int64
}

var transformationRegistryCacheV3 = struct {
	sync.Mutex
	values map[transformationRegistryCacheKeyV3]*TransformationRegistryV3
}{values: make(map[transformationRegistryCacheKeyV3]*TransformationRegistryV3)}

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
	entries map[string]TransformationSpecV3
}

func ProbeTransformationRegistryV3(ctx context.Context, ffmpegPath string) *TransformationRegistryV3 {
	return ProbeTransformationRegistryForExecutorV3(ctx, ffmpegPath, HWAccelNone)
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
	transformationRegistryCacheV3.Lock()
	if cached := transformationRegistryCacheV3.values[key]; cached != nil {
		transformationRegistryCacheV3.Unlock()
		return cached
	}
	transformationRegistryCacheV3.Unlock()
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
	registry := NewTransformationRegistryV3([]TransformationSpecV3{
		{Name: TransformationServerDV7HDR10V3, RecipeVersion: "1", Available: bytes.Contains(bsfs, []byte("dovi_rpu")), RequiredCapability: "ffmpeg_bsf:dovi_rpu", PromisedDynamicRange: DynamicRangeHDR10V3, ValidatedClaims: DV7ToHDR10ClaimsV3(), TerminalReason: TerminalDVConversionUnsupportedV3},
		{Name: TransformationAudioToAACV3, RecipeVersion: "1", Available: ffmpegErr == nil && bytes.Contains(encoders, []byte(" aac ")), RequiredCapability: "ffmpeg_encoder:aac", ValidatedClaims: []string{ClaimAudioDecodeV3}, TerminalReason: TerminalAudioConversionUnsupportedV3},
		{Name: TransformationVideoToH264V3, RecipeVersion: TransformationVideoToH264RecipeVersionV3, Available: ffmpegErr == nil && h264EncoderAvailableForExecutorV3(encoders, resolvedHWAccel) && h264FiltersAvailableForExecutorV3(filters, resolvedHWAccel), RequiredCapability: "ffmpeg_encoder_and_filters:h264", PromisedDynamicRange: DynamicRangeSDRV3, ValidatedClaims: []string{ClaimH264DecodeV3}, TerminalReason: TerminalVideoConversionUnsupportedV3},
	})
	// Memoize only a fully-observed answer. Each probe above carries its own
	// three-second deadline, and a subprocess that timed out, failed to fork
	// under memory pressure, or was killed describes the load on this host —
	// not the binary's capabilities. Caching that would advertise "no H.264
	// encoder" for the lifetime of the process (the key is the resolved path
	// plus its mtime/size, which do not change) and turn one slow moment into
	// a permanent transcode outage on this executor.
	probesObserved := bsfErr == nil && encoderErr == nil && filterErr == nil
	if ctx.Err() == nil && probesObserved {
		transformationRegistryCacheV3.Lock()
		transformationRegistryCacheV3.values[key] = registry
		transformationRegistryCacheV3.Unlock()
	}
	if !probesObserved {
		slog.WarnContext(ctx, "ffmpeg transformation probe incomplete; not memoizing",
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
		if !bytes.Contains(filters, []byte(name)) {
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
	return bytes.Contains(encoders, []byte(encoder)) && bytes.Contains(encoders, []byte(encoderH264Software))
}

func NewTransformationRegistryV3(specs []TransformationSpecV3) *TransformationRegistryV3 {
	r := &TransformationRegistryV3{entries: make(map[string]TransformationSpecV3, len(specs))}
	for _, spec := range specs {
		if spec.Name != "" {
			r.entries[spec.Name] = spec
		}
	}
	return r
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
	return NewTransformationRegistryV3(specs)
}

func (r *TransformationRegistryV3) Advertised() []TransformationV3 {
	if r == nil {
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
