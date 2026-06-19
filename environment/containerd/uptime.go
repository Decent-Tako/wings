package containerd

import (
	"context"
	"time"
)

func (e *Environment) setStartedAt(ctx context.Context, t time.Time) {
	t = t.UTC()
	e.mu.Lock()
	e.startedAt = t
	e.mu.Unlock()
	e.persistStartedAt(ctx, t)
}

func (e *Environment) StartedAt() (time.Time, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.startedAt, !e.startedAt.IsZero()
}

func (e *Environment) RestoreStartedAt(t time.Time) {
	if t.IsZero() {
		return
	}
	e.mu.Lock()
	e.startedAt = t.UTC()
	e.mu.Unlock()
}

func (e *Environment) startedAtOrRestore(ctx context.Context, fallback time.Time) time.Time {
	e.mu.RLock()
	startedAt := e.startedAt
	e.mu.RUnlock()
	if !startedAt.IsZero() {
		return startedAt
	}

	if restored, ok := e.startedAtFromLabel(ctx); ok {
		e.mu.Lock()
		e.startedAt = restored
		e.mu.Unlock()
		return restored
	}

	if fallback.IsZero() {
		return time.Time{}
	}
	e.setStartedAt(ctx, fallback)
	return fallback
}

func (e *Environment) startedAtFromLabel(ctx context.Context) (time.Time, bool) {
	c, err := e.container(ctx)
	if err != nil {
		return time.Time{}, false
	}
	labels, err := c.Labels(e.context(ctx))
	if err != nil {
		return time.Time{}, false
	}
	raw := labels[labelStartedAt]
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		e.log().WithField("value", raw).WithField("error", err).Warn("invalid containerd started_at label")
		return time.Time{}, false
	}
	return t, true
}

func (e *Environment) persistStartedAt(ctx context.Context, t time.Time) {
	c, err := e.container(ctx)
	if err != nil {
		e.log().WithField("error", err).Debug("could not load container to persist started_at label")
		return
	}
	labels, err := c.Labels(e.context(ctx))
	if err != nil {
		e.log().WithField("error", err).Debug("could not read container labels to persist started_at")
		return
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelStartedAt] = t.UTC().Format(time.RFC3339Nano)
	if _, err := c.SetLabels(e.context(ctx), labels); err != nil {
		e.log().WithField("error", err).Warn("could not persist containerd started_at label")
	}
}
