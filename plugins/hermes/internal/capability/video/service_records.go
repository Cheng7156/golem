package video

import (
	"crypto/rand"
	"encoding/base64"
	"time"
)

func (s *service) purgeExpiredLocked(now time.Time) {
	for id, record := range s.records {
		if !now.Before(record.public.ExpiresAt) && record.inflight == nil {
			delete(s.records, id)
		}
	}
}

func (s *service) enforceCapacityLocked() {
	for len(s.records) > s.maxRecords {
		var oldestID string
		var oldest time.Time
		for id, record := range s.records {
			if record.inflight != nil {
				continue
			}
			if oldestID == "" || record.created.Before(oldest) {
				oldestID, oldest = id, record.created
			}
		}
		if oldestID == "" {
			return
		}
		delete(s.records, oldestID)
	}
}

func newVideoCandidateID() (string, error) {
	var data [18]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return "vid_" + base64.RawURLEncoding.EncodeToString(data[:]), nil
}
