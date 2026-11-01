package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/isLinXu/fmux/internal/supervisor"
)

// DrainAndStop shuts the gateway down in the only order that does not corrupt
// a live response.
//
// The obvious order — stop the child processes, then close the listener — is
// wrong. The children are what in-flight requests are reading from, so killing
// them first turns every generation that is halfway through into a truncated
// stream with no error event. A client cannot distinguish that from a network
// glitch, and a supervised backend that is stopped mid-request is worse still:
// the process is gone, so the retry starts a 20-second cold load.
//
// So: stop accepting new work, let running handlers finish, and only then stop
// the processes nobody is talking to any more.
func DrainAndStop(log *slog.Logger, httpSrv *http.Server, srv *Server, sup *supervisor.Supervisor, drain time.Duration) {
	if log == nil {
		log = slog.Default()
	}
	log.Info("shutting down", "inflight", srv.Inflight(), "drain_timeout", drain.String())

	// Shutdown closes the listeners and returns once every connection is
	// idle, or when the deadline expires. It does not force-close, which is
	// why the deadline path has to call Close explicitly.
	ctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		// The deadline expired with handlers still running. Close cuts them
		// off, which the client will see as a truncated stream — the one
		// outcome this function exists to avoid, so say so loudly.
		log.Warn("drain deadline hit, cutting in-flight requests",
			"inflight", srv.Inflight(), "drain_timeout", drain.String(), "err", err)
		_ = httpSrv.Close()
	}
	log.Info("http drained", "inflight", srv.Inflight())

	if sup == nil {
		return
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	sup.Shutdown(stopCtx)
	log.Info("stopped")
}
