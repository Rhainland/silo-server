package notifications

import (
	"encoding/json"

	"github.com/Silo-Server/silo-server/internal/models"
)

const storageTransitionJobType = "storage_transition"

type storageTransitionEventResult struct {
	Phase                 string `json:"phase"`
	VerifiedObjects       int    `json:"verified_objects"`
	FailureCategory       string `json:"failure_category,omitempty"`
	ManualRestartRequired bool   `json:"manual_restart_required"`
}

// SafeStorageTransitionJob removes diagnostic storage details from an outbound
// administrator job. The original row keeps its diagnostic detail.
func SafeStorageTransitionJob(job *models.AdminJob) *models.AdminJob {
	if job == nil || job.JobType != storageTransitionJobType {
		return job
	}
	var raw struct {
		Phase                 string `json:"phase"`
		VerifiedObjects       int    `json:"verified_objects"`
		CopiedObjects         int    `json:"copied_objects"`
		FailureCategory       string `json:"failure_category"`
		ManualRestartRequired bool   `json:"manual_restart_required"`
	}
	_ = json.Unmarshal(job.ResultPayload, &raw)
	result := storageTransitionEventResult{
		VerifiedObjects:       max(raw.VerifiedObjects, raw.CopiedObjects, 0),
		ManualRestartRequired: raw.ManualRestartRequired,
	}
	switch job.Status {
	case "failed":
		result.Phase = "failed"
		switch raw.FailureCategory {
		case "preparation_failed", "target_check_failed", "copy_failed", "verification_failed", "commit_failed":
			result.FailureCategory = raw.FailureCategory
		default:
			result.FailureCategory = "unknown"
		}
	case "cancelled":
		result.Phase = "canceled"
	case "completed":
		if raw.Phase == "restart_pending" || raw.ManualRestartRequired {
			result.Phase = "restart_pending"
		} else {
			result.Phase = "completed"
		}
	default:
		switch raw.Phase {
		case "checking_target", "copying", "verifying", "committing", "restart_pending":
			result.Phase = raw.Phase
		default:
			if job.Status == "queued" {
				result.Phase = "queued"
			} else {
				result.Phase = "checking_target"
			}
		}
	}
	resultPayload, _ := json.Marshal(result)
	return &models.AdminJob{
		ID: job.ID, JobType: job.JobType, Status: job.Status,
		CreatedByUserID: job.CreatedByUserID,
		RequestPayload:  json.RawMessage(`{}`), ResultPayload: resultPayload,
		ProgressCurrent: result.VerifiedObjects,
		RequestedAt:     job.RequestedAt, StartedAt: job.StartedAt, CompletedAt: job.CompletedAt,
		HeartbeatAt: job.HeartbeatAt, ExpiresAt: job.ExpiresAt, UpdatedAt: job.UpdatedAt,
	}
}
