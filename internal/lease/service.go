package lease

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

var ErrLeaseStillActive = errors.New("lease is still active")

// Service journals ownership changes and exposes admission authorization.
type Service struct {
	registry  *Registry
	admission *admission.Controller
	recorder  *journal.Recorder
	now       func() time.Time
}

func NewService(registry *Registry, admissionController *admission.Controller, recorder *journal.Recorder, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{registry: registry, admission: admissionController, recorder: recorder, now: now}
}

func (s *Service) Assign(ctx context.Context, chamberID string, vesselID uuid.UUID, owner string, ttl time.Duration) (model.Lease, error) {
	lease, err := s.registry.Assign(vesselID, owner, ttl, s.now())
	if err != nil {
		return model.Lease{}, err
	}
	_, err = s.recorder.Record(ctx, chamberID, model.EventLeaseChanged, uuid.Nil, 0, lease)
	return lease, err
}

func (s *Service) Heartbeat(ctx context.Context, chamberID string, vesselID uuid.UUID, owner string, token uint64, ttl time.Duration) (model.Lease, error) {
	lease, err := s.registry.Renew(vesselID, owner, token, ttl, s.now())
	if err != nil {
		return model.Lease{}, err
	}
	_, err = s.recorder.Record(ctx, chamberID, model.EventLeaseChanged, uuid.Nil, token, lease)
	return lease, err
}

func (s *Service) Authorized(vesselID uuid.UUID, owner string, token uint64) bool {
	lease, ok := s.registry.Get(vesselID)
	return ok && s.admission.OwnerAllowed(vesselID, owner, token, lease, s.now())
}

// Active returns leases that had not expired at the given time.
func (s *Service) Active(now time.Time) []model.Lease {
	var result []model.Lease
	for _, leaseValue := range s.registry.Snapshot() {
		if leaseValue.Active(now) {
			result = append(result, leaseValue)
		}
	}
	return result
}

func (s *Service) Release(ctx context.Context, chamberID string, vesselID uuid.UUID, owner string, token uint64) error {
	if !s.registry.Release(vesselID, owner, token) {
		return errors.New("lease owner or fencing token is stale")
	}
	_, err := s.recorder.Record(ctx, chamberID, model.EventLeaseChanged, uuid.Nil, token, map[string]any{
		"vessel_id": vesselID, "owner": owner, "released": true,
	})
	return err
}
