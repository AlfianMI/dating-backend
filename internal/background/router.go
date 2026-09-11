package background

import (
	"context"
	"encoding/json"
	"log"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/pipigendut/dating-backend/internal/entities"
	"github.com/pipigendut/dating-backend/internal/repository"
)

// Base payload that all our jobs must include to track the job ID.
type BaseJobPayload struct {
	JobID string `json:"job_id"`
}

type JobRouter struct {
	mux     *asynq.ServeMux
	jobRepo repository.JobRepository
}

func NewJobRouter(jobRepo repository.JobRepository) *JobRouter {
	mux := asynq.NewServeMux()
	router := &JobRouter{
		mux:     mux,
		jobRepo: jobRepo,
	}

	// Register our middleware to handle DB statuses automatically
	mux.Use(router.trackingMiddleware)

	return router
}

func (r *JobRouter) Mux() *asynq.ServeMux {
	return r.mux
}

// trackingMiddleware wraps every job execution to update the jobs table in PostgreSQL.
func (r *JobRouter) trackingMiddleware(h asynq.Handler) asynq.Handler {
	return asynq.HandlerFunc(func(ctx context.Context, t *asynq.Task) error {
		// 1. Extract internal JobID from payload
		var basePayload BaseJobPayload
		if err := json.Unmarshal(t.Payload(), &basePayload); err != nil {
			log.Printf("Failed to unmarshal base payload for task %s: %v", t.Type(), err)
			return h.ProcessTask(ctx, t) // Not tracked by DB
		}

		jobID, err := uuid.Parse(basePayload.JobID)
		if err != nil {
			return h.ProcessTask(ctx, t) // Not tracked by DB
		}

		// 2. Mark as processing and increment attempt
		if err := r.jobRepo.IncrementJobAttempt(ctx, jobID); err != nil {
			return err
		}

		if err := r.jobRepo.UpdateJobStatus(
			ctx,
			jobID,
			entities.JobStatusProcessing,
			nil,
		); err != nil {
			return err
		}

		// 3. Execute the actual handler module
		handlerErr := h.ProcessTask(ctx, t)

		// 4. Handle Result
		if handlerErr != nil {
			errMsg := handlerErr.Error()

			if err := r.jobRepo.UpdateJobStatus(
				ctx,
				jobID,
				entities.JobStatusFailed,
				&errMsg,
			); err != nil {
				log.Printf("Failed to update job %s to failed: %v", jobID, err)
			}

			return handlerErr // Return error to Asynq so it retries
		}

		// 5. Success
		if err := r.jobRepo.UpdateJobStatus(
			ctx,
			jobID,
			entities.JobStatusCompleted,
			nil,
		); err != nil {
			return err
		}

		return nil
	})
}

// RegisterHandler binds a task type to a specific handler function.
func (r *JobRouter) RegisterHandler(taskType string, handler asynq.HandlerFunc) {
	r.mux.HandleFunc(taskType, handler)
}
