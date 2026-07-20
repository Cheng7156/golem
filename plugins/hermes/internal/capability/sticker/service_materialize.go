package sticker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golem_plugin_hermes/internal/domain"
)

type materializeCall struct {
	done   chan struct{}
	output domain.EmojiOutput
	err    error
}

type materializeLease struct {
	call        *materializeCall
	provider    Provider
	candidate   ProviderCandidate
	description string
	cached      *domain.EmojiOutput
	owner       bool
}

func (s *service) Materialize(
	ctx context.Context,
	scope Scope,
	candidateID string,
) (domain.EmojiOutput, error) {
	if err := scope.validate(); err != nil {
		return domain.EmojiOutput{}, err
	}
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return domain.EmojiOutput{}, ErrCandidateNotFound
	}
	lease, err := s.acquireMaterialization(scope, candidateID)
	if err != nil {
		return domain.EmojiOutput{}, err
	}
	if lease.cached != nil {
		return cloneEmojiOutput(*lease.cached), nil
	}
	if !lease.owner {
		return waitForMaterialization(ctx, lease.call)
	}
	return s.runMaterialization(ctx, candidateID, lease)
}

func (s *service) acquireMaterialization(
	scope Scope,
	candidateID string,
) (materializeLease, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[candidateID]
	if record == nil {
		return materializeLease{}, ErrCandidateNotFound
	}
	if !now.Before(record.public.ExpiresAt) {
		s.deleteRecordLocked(candidateID)
		return materializeLease{}, ErrCandidateExpired
	}
	if record.scope != scope {
		return materializeLease{}, ErrCandidateScope
	}
	record.lastAccess = now
	if record.materialized != nil {
		return materializeLease{cached: record.materialized}, nil
	}
	if record.inflight != nil {
		return materializeLease{call: record.inflight}, nil
	}
	provider := s.providers[record.provider]
	if provider == nil {
		return materializeLease{}, fmt.Errorf("%w: %s", ErrProviderNotFound, record.provider)
	}
	call := &materializeCall{done: make(chan struct{})}
	record.inflight = call
	return materializeLease{
		call: call, provider: provider, candidate: record.value,
		description: record.public.Description, owner: true,
	}, nil
}

func (s *service) runMaterialization(
	ctx context.Context,
	candidateID string,
	lease materializeLease,
) (domain.EmojiOutput, error) {
	output, err := lease.provider.Materialize(ctx, lease.candidate)
	if err == nil {
		err = normalizeMaterializedOutput(&output, lease.description)
	}
	return s.publishMaterialization(candidateID, lease.call, output, err)
}

func normalizeMaterializedOutput(output *domain.EmojiOutput, fallbackDescription string) error {
	if len(output.Data) == 0 {
		return errors.New("sticker provider materialized empty data")
	}
	output.URL = ""
	mimeType, err := imageMIME(output.Data)
	if err != nil {
		return err
	}
	output.MIMEType = mimeType
	if strings.TrimSpace(output.Description) == "" {
		output.Description = strings.TrimSpace(fallbackDescription)
	}
	return nil
}

func (s *service) publishMaterialization(
	candidateID string,
	call *materializeCall,
	output domain.EmojiOutput,
	materializeErr error,
) (domain.EmojiOutput, error) {
	s.mu.Lock()
	result, err := s.publishMaterializationLocked(candidateID, call, output, materializeErr)
	call.output = result
	call.err = err
	close(call.done)
	s.mu.Unlock()
	return cloneEmojiOutput(result), err
}

func (s *service) publishMaterializationLocked(
	candidateID string,
	call *materializeCall,
	output domain.EmojiOutput,
	materializeErr error,
) (domain.EmojiOutput, error) {
	record := s.records[candidateID]
	if record == nil || record.inflight != call {
		return domain.EmojiOutput{}, ErrCandidateNotFound
	}
	record.inflight = nil
	now := s.now()
	if !now.Before(record.public.ExpiresAt) {
		s.deleteRecordLocked(candidateID)
		return domain.EmojiOutput{}, ErrCandidateExpired
	}
	if materializeErr != nil {
		return domain.EmojiOutput{}, materializeErr
	}
	if err := s.makeCacheRoomLocked(candidateID, int64(len(output.Data)), now); err != nil {
		return domain.EmojiOutput{}, err
	}
	cached := cloneEmojiOutput(output)
	record.materialized = &cached
	record.lastAccess = now
	s.cacheBytes += int64(len(cached.Data))
	return cached, nil
}

func (s *service) makeCacheRoomLocked(candidateID string, size int64, now time.Time) error {
	s.purgeExpiredLocked(now)
	if size > s.maxCacheBytes {
		return ErrMaterializedCacheFull
	}
	for size > s.maxCacheBytes-s.cacheBytes {
		victim := s.oldestMaterializedLocked(candidateID)
		if victim == "" {
			return ErrMaterializedCacheFull
		}
		s.deleteRecordLocked(victim)
	}
	return nil
}

func (s *service) oldestMaterializedLocked(excludedID string) string {
	var victimID string
	var oldest time.Time
	for candidateID, record := range s.records {
		if candidateID == excludedID || record.materialized == nil {
			continue
		}
		if victimID == "" || record.lastAccess.Before(oldest) {
			victimID = candidateID
			oldest = record.lastAccess
		}
	}
	return victimID
}

func waitForMaterialization(
	ctx context.Context,
	call *materializeCall,
) (domain.EmojiOutput, error) {
	select {
	case <-ctx.Done():
		return domain.EmojiOutput{}, ctx.Err()
	case <-call.done:
		return cloneEmojiOutput(call.output), call.err
	}
}

func cloneEmojiOutput(output domain.EmojiOutput) domain.EmojiOutput {
	clone := output
	clone.Data = append([]byte(nil), output.Data...)
	return clone
}
