package video

import (
	"context"
	"strings"

	"golem_plugin_hermes/internal/domain"
)

func (s *service) Select(
	ctx context.Context,
	scope Scope,
	candidateID string,
) (domain.VideoOutput, error) {
	if err := scope.validate(); err != nil {
		return domain.VideoOutput{}, err
	}
	record, call, owner, err := s.reserveSelection(scope, strings.TrimSpace(candidateID))
	if err != nil {
		return domain.VideoOutput{}, err
	}
	if !owner {
		return s.waitForPreparation(ctx, scope.RunID, call)
	}
	prepareCtx, cancel := context.WithTimeout(ctx, s.prepareTimeout)
	defer cancel()
	if err := s.acquirePrepareSlot(prepareCtx); err != nil {
		s.finishPreparation(preparationOutcome{
			runID: scope.RunID, record: record, call: call, err: err,
		})
		return domain.VideoOutput{}, err
	}
	defer func() { <-s.prepareSlots }()
	output, prepareErr := s.prepareRecord(prepareCtx, record)
	s.finishPreparation(preparationOutcome{
		runID: scope.RunID, record: record, call: call, output: output, err: prepareErr,
	})
	return output, prepareErr
}

func (s *service) acquirePrepareSlot(ctx context.Context) error {
	select {
	case s.prepareSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *service) reserveSelection(
	scope Scope,
	candidateID string,
) (*candidateRecord, *prepareCall, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[candidateID]
	if record == nil {
		return nil, nil, false, ErrCandidateNotFound
	}
	if record.scope != scope {
		return nil, nil, false, ErrCandidateScope
	}
	if !s.now().Before(record.public.ExpiresAt) {
		return nil, nil, false, ErrCandidateExpired
	}
	if s.runCounts[scope.RunID] >= s.maxPerRun {
		return nil, nil, false, ErrRunVideoLimit
	}
	s.runCounts[scope.RunID]++
	if record.prepared != nil {
		call := &prepareCall{done: make(chan struct{}), output: *record.prepared}
		close(call.done)
		return record, call, false, nil
	}
	if record.inflight != nil {
		return record, record.inflight, false, nil
	}
	call := &prepareCall{done: make(chan struct{})}
	record.inflight = call
	return record, call, true, nil
}

func (s *service) waitForPreparation(
	ctx context.Context,
	runID string,
	call *prepareCall,
) (domain.VideoOutput, error) {
	select {
	case <-ctx.Done():
		s.releaseRunSlot(runID)
		return domain.VideoOutput{}, ctx.Err()
	case <-call.done:
		if call.err != nil {
			s.releaseRunSlot(runID)
		}
		return call.output, call.err
	}
}

func (s *service) prepareRecord(
	ctx context.Context,
	record *candidateRecord,
) (domain.VideoOutput, error) {
	resolved, err := record.provider.Resolve(ctx, record.value)
	if err != nil {
		return domain.VideoOutput{}, &PreparationError{
			ProviderID: record.provider.ID(), FallbackURL: record.provider.FallbackURL(record.value), Cause: err,
		}
	}
	output, err := s.pipeline.Prepare(ctx, resolved)
	if err != nil {
		return domain.VideoOutput{}, &PreparationError{
			ProviderID: record.provider.ID(), FallbackURL: resolved.FallbackURL, Cause: err,
		}
	}
	return output, nil
}

type preparationOutcome struct {
	runID  string
	record *candidateRecord
	call   *prepareCall
	output domain.VideoOutput
	err    error
}

func (s *service) finishPreparation(outcome preparationOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcome.call.output, outcome.call.err = outcome.output, outcome.err
	if outcome.err == nil {
		cached := outcome.output
		outcome.record.prepared = &cached
	} else {
		s.runCounts[outcome.runID]--
	}
	outcome.record.inflight = nil
	close(outcome.call.done)
}

func (s *service) releaseRunSlot(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runCounts[runID] > 0 {
		s.runCounts[runID]--
	}
}
