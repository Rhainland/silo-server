package apiv2

import (
	"context"
	"net/http"
)

type AdminEpisodeMarkersService interface {
	RefreshEpisodeMarkers(context.Context, string, string) (string, error)
}
type AdminEpisodeMarkersInput struct {
	ID string `path:"id" minLength:"1" maxLength:"512"`
}
type AdminEpisodeMarkersStatus struct {
	Status string `json:"status" enum:"queued,already_running" doc:"Process-local analysis, not a persisted job."`
}
type AdminEpisodeMarkersOutput struct{ Body AdminEpisodeMarkersStatus }

const (
	refreshAdminEpisodeMarkersOperation  = "refreshAdminEpisodeMarkers"
	redetectAdminEpisodeIntroOperation   = "redetectAdminEpisodeIntro"
	redetectAdminEpisodeMarkersOperation = "redetectAdminEpisodeMarkers"
)

// episodeMarkersSummary describes refresh-markers and redetect-intro, which
// carry the v1 routes' behavior.
const episodeMarkersSummary = "Refresh episode markers using configured sources, or explicitly rerun local intro detection."

func registerAdminCatalogIntro(reg *Registry) {
	for _, action := range []struct{ suffix, id, action, summary string }{
		{"refresh-markers", refreshAdminEpisodeMarkersOperation, "refresh-v2", episodeMarkersSummary},
		{"redetect-intro", redetectAdminEpisodeIntroOperation, "redetect", episodeMarkersSummary},
		{"redetect-markers", redetectAdminEpisodeMarkersOperation, "redetect-v2",
			"Explicitly rerun local detection of the episode markers the detection settings select: intros, credits, or both."},
	} {
		op := Operation{Operation: humaOp(http.MethodPost, Prefix+"/admin/items/{id}/"+action.suffix, action.id, "admin-catalog", action.summary), Class: ClassActingAdmin, ServiceBacked: true, DemoRestricted: true, RetrySafety: RetrySafetyNonRetryable}
		op.DefaultStatus = http.StatusAccepted
		Register(reg, op, func(ctx context.Context, in *AdminEpisodeMarkersInput) (*AdminEpisodeMarkersOutput, error) {
			if reg.deps.AdminEpisodeMarkers == nil {
				return nil, unavailable("episode marker analysis")
			}
			status, err := reg.deps.AdminEpisodeMarkers.RefreshEpisodeMarkers(ctx, in.ID, action.action)
			if err != nil {
				return nil, collectionProblem(err)
			}
			return &AdminEpisodeMarkersOutput{Body: AdminEpisodeMarkersStatus{Status: status}}, nil
		})
	}
}
