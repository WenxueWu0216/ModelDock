// Package graph is the gqlgen-generated GraphQL layer. Resolvers are thin:
// they read the authenticated user from the context and call the service.
package graph

//go:generate go run github.com/99designs/gqlgen generate

import (
	"context"

	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/service"
)

// Resolver is the root resolver.
type Resolver struct {
	Svc *service.Service
}

func (r *modelResolver) current(ctx context.Context, m *domain.Model, env domain.Environment) (*domain.Deployment, error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	st, err := r.Svc.Status(ctx, u, m.Name)
	if err != nil {
		return nil, err
	}
	return st.Current[env], nil
}

func (r *mutationResolver) decide(ctx context.Context, id string, approve bool, comment *string) (*domain.Approval, error) {
	u, err := user(ctx)
	if err != nil {
		return nil, err
	}
	a, err := r.Svc.Decide(ctx, u, id, approve, str(comment))
	if err != nil {
		return nil, err
	}
	return &a, nil
}
