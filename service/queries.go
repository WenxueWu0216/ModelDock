package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/wenxuewu0216/modeldock/auth"
	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/events"
)

// ModelStatus is the current deployment of a model in each environment.
type ModelStatus struct {
	Model   domain.Model                              `json:"model"`
	Current map[domain.Environment]*domain.Deployment `json:"current"`
}

func read(u domain.User) error { return auth.Require(u, auth.PermRead) }

// ListModels returns all models.
func (s *Service) ListModels(ctx context.Context, u domain.User) ([]domain.Model, error) {
	if err := read(u); err != nil {
		return nil, err
	}
	return s.Store.ListModels(ctx)
}

// GetModel returns a model by name.
func (s *Service) GetModel(ctx context.Context, u domain.User, name string) (domain.Model, error) {
	if err := read(u); err != nil {
		return domain.Model{}, err
	}
	return s.Store.GetModel(ctx, name)
}

// ListVersions returns a model's versions, newest first.
func (s *Service) ListVersions(ctx context.Context, u domain.User, model string) ([]domain.ModelVersion, error) {
	if err := read(u); err != nil {
		return nil, err
	}
	if _, err := s.Store.GetModel(ctx, model); err != nil {
		return nil, err
	}
	return s.Store.ListVersions(ctx, model)
}

// GetVersion returns one version.
func (s *Service) GetVersion(ctx context.Context, u domain.User, model, version string) (domain.ModelVersion, error) {
	if err := read(u); err != nil {
		return domain.ModelVersion{}, err
	}
	return s.Store.GetVersion(ctx, model, version)
}

// Diff compares two versions of a model.
func (s *Service) Diff(ctx context.Context, u domain.User, model, from, to string) (domain.VersionDiff, error) {
	if err := read(u); err != nil {
		return domain.VersionDiff{}, err
	}
	a, err := s.Store.GetVersion(ctx, model, from)
	if err != nil {
		return domain.VersionDiff{}, fmt.Errorf("service: diff: %w", err)
	}
	b, err := s.Store.GetVersion(ctx, model, to)
	if err != nil {
		return domain.VersionDiff{}, fmt.Errorf("service: diff: %w", err)
	}
	return domain.Diff(a, b), nil
}

// Status returns the current revision per environment.
func (s *Service) Status(ctx context.Context, u domain.User, model string) (ModelStatus, error) {
	if err := read(u); err != nil {
		return ModelStatus{}, err
	}
	m, err := s.Store.GetModel(ctx, model)
	if err != nil {
		return ModelStatus{}, err
	}
	st := ModelStatus{Model: m, Current: map[domain.Environment]*domain.Deployment{}}
	for _, env := range []domain.Environment{domain.EnvStaging, domain.EnvProd} {
		d, err := s.Store.CurrentDeployment(ctx, m.ID, env)
		if errors.Is(err, domain.ErrNotFound) {
			st.Current[env] = nil
			continue
		}
		if err != nil {
			return ModelStatus{}, err
		}
		st.Current[env] = &d
	}
	return st, nil
}

// History returns deployment revisions of a model, newest first.
func (s *Service) History(ctx context.Context, u domain.User, model string, env domain.Environment, limit int) ([]domain.Deployment, error) {
	if err := read(u); err != nil {
		return nil, err
	}
	if _, err := s.Store.GetModel(ctx, model); err != nil {
		return nil, err
	}
	return s.Store.ListDeployments(ctx, model, env, limit)
}

// GetDeployment returns one deployment revision.
func (s *Service) GetDeployment(ctx context.Context, u domain.User, id string) (domain.Deployment, error) {
	if err := read(u); err != nil {
		return domain.Deployment{}, err
	}
	return s.Store.GetDeployment(ctx, id)
}

// ListApprovals returns promotion requests, optionally by status.
func (s *Service) ListApprovals(ctx context.Context, u domain.User, status domain.ApprovalStatus) ([]domain.Approval, error) {
	if err := read(u); err != nil {
		return nil, err
	}
	return s.Store.ListApprovals(ctx, status)
}

// GetApproval returns one approval.
func (s *Service) GetApproval(ctx context.Context, u domain.User, id string) (domain.Approval, error) {
	if err := read(u); err != nil {
		return domain.Approval{}, err
	}
	return s.Store.GetApproval(ctx, id)
}

// Audit returns audit events, newest first.
func (s *Service) Audit(ctx context.Context, u domain.User, model string, beforeSeq int64, limit int) ([]events.Event, error) {
	if err := read(u); err != nil {
		return nil, err
	}
	return s.Store.ListAudit(ctx, model, beforeSeq, limit)
}

// Dashboard returns the health dashboard read model.
func (s *Service) Dashboard(ctx context.Context, u domain.User) ([]events.ModelHealth, error) {
	if err := read(u); err != nil {
		return nil, err
	}
	return s.Store.ListDashboard(ctx)
}
