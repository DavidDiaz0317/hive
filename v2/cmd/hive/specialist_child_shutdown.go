package main

import (
	"context"
	"log/slog"
	"time"
)

const ordinarySpecialistChildShutdownTimeout = 30 * time.Second

type specialistChildShutdowner interface {
	ShutdownSpecialistChildren(context.Context) error
}

// shutdownOrdinaryVisualRuntime preserves the single-writer fence until every
// ephemeral child is reaped. The application context is canceled first so no
// new work can begin; ownership is released last so another process cannot
// acquire the runtime while a child from this owner remains live.
func shutdownOrdinaryVisualRuntime(cancel context.CancelFunc, manager specialistChildShutdowner, releaseOwnership func(), logger *slog.Logger) {
	if cancel != nil {
		cancel()
	}
	shutdownOrdinarySpecialistChildren(manager, logger)
	if releaseOwnership != nil {
		releaseOwnership()
	}
}

// shutdownOrdinarySpecialistChildren is deliberately typed to the ephemeral
// child-only shutdown surface. It cannot stop or restart persistent agents.
func shutdownOrdinarySpecialistChildren(manager specialistChildShutdowner, logger *slog.Logger) {
	if manager == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ordinarySpecialistChildShutdownTimeout)
	defer cancel()
	if err := manager.ShutdownSpecialistChildren(ctx); err != nil && logger != nil {
		logger.Warn("shutdown ordinary Hive specialist children", "error", err)
	}
}
