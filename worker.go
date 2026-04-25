package photopicker

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// DefaultPollInterval is how often Worker.Run polls the ImportStore for new
// jobs when WorkerConfig.PollInterval is zero.
const DefaultPollInterval = 2 * time.Second

// DefaultJobTimeout bounds each ProcessJob invocation when WorkerConfig.JobTimeout
// is zero. 35 minutes matches the 5m picker-poll window plus 30m per-item budget.
const DefaultJobTimeout = 35 * time.Minute

// WorkerConfig is the constructor input for Worker.
//
// Client is required.
//
// Optional:
//   - PollInterval: how often Run polls for pending jobs (default 2s).
//   - JobTimeout: per-job timeout applied to ProcessJob (default 35m).
//   - Logger: structured logger; defaults to the Client's logger.
type WorkerConfig struct {
	Client       *Client
	PollInterval time.Duration
	JobTimeout   time.Duration
	Logger       *slog.Logger
}

// Worker processes queued import jobs. Claims use whatever locking semantics
// the ImportStore provides (the bundled Postgres store uses FOR UPDATE SKIP
// LOCKED, so multiple replicas can run safely).
type Worker struct {
	client       *Client
	pollInterval time.Duration
	jobTimeout   time.Duration
	logger       *slog.Logger
}

// NewWorker builds a Worker. It does NOT start any goroutines — call Run to do so.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("%w: Client is required", ErrInvalidConfig)
	}
	w := &Worker{
		client:       cfg.Client,
		pollInterval: cfg.PollInterval,
		jobTimeout:   cfg.JobTimeout,
		logger:       cfg.Logger,
	}
	if w.pollInterval == 0 {
		w.pollInterval = DefaultPollInterval
	}
	if w.jobTimeout == 0 {
		w.jobTimeout = DefaultJobTimeout
	}
	if w.logger == nil {
		w.logger = cfg.Client.logger
	}
	return w, nil
}

// Run blocks, polling the ImportStore on a ticker and draining pending jobs,
// until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.DrainOnce(ctx)
		}
	}
}

// DrainOnce performs a single drain pass, claiming and processing jobs until
// the queue is empty (or an error occurs). Useful in tests and for serverless
// / cron deployments.
func (w *Worker) DrainOnce(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := w.client.imports.ClaimNextPending(ctx)
		if err != nil {
			w.logger.Warn("photopicker: claim import job", "err", err)
			return
		}
		if job == nil {
			return
		}
		if err := w.ProcessJob(ctx, job); err != nil {
			w.logger.Warn("photopicker: process job", "job", job.ID, "err", err)
		}
	}
}

// ProcessJob runs a single claimed job to terminal state, applying the
// per-job timeout. Returns the error used to mark the job failed (nil on
// success).
func (w *Worker) ProcessJob(ctx context.Context, job *ImportJob) error {
	jobCtx, cancel := context.WithTimeout(ctx, w.jobTimeout)
	defer cancel()

	items, err := listSessionMediaItems(jobCtx, w.client.httpClient, w.client.authorizer(), job.UserID, job.SessionID)
	if err != nil {
		_ = w.client.imports.MarkFailed(jobCtx, job.ID, err.Error())
		return fmt.Errorf("list media items: %w", err)
	}

	if err := w.client.imports.SetTotal(jobCtx, job.ID, len(items)); err != nil {
		w.logger.Warn("photopicker: set total", "job", job.ID, "err", err)
	}

	for _, item := range items {
		if err := w.importOne(jobCtx, job.UserID, job.ID, item); err != nil {
			w.logger.Warn("photopicker: import item", "job", job.ID, "item", item.ID, "err", err)
			_ = w.client.imports.RecordItemFailure(jobCtx, job.ID)
		}
	}

	if err := w.client.imports.MarkComplete(jobCtx, job.ID); err != nil {
		w.logger.Warn("photopicker: mark complete", "job", job.ID, "err", err)
	}
	// Best-effort session cleanup.
	_ = deletePickerSession(jobCtx, w.client.httpClient, w.client.authorizer(), job.UserID, job.SessionID)
	return nil
}

func (w *Worker) importOne(ctx context.Context, userID, jobID string, item mediaItem) error {
	photo, err := downloadMediaItem(ctx, w.client.httpClient, w.client.authorizer(), userID, item, w.client.downloadCap, w.client.maxDecodedBytes)
	if err != nil {
		return err
	}
	savedID, err := w.client.sink.SavePhoto(ctx, userID, jobID, photo)
	if err != nil {
		return fmt.Errorf("sink: %w", err)
	}
	return w.client.imports.RecordItemSuccess(ctx, jobID, savedID)
}
