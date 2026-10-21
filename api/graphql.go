package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/wenxuewu0216/modeldock/api/graph"
	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/service"
)

// errorCodes are exposed as extensions.code so GraphQL clients can branch on
// the same categories REST clients get as HTTP status codes.
func errorCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrUnauthorized):
		return "UNAUTHENTICATED"
	case errors.Is(err, domain.ErrForbidden):
		return "FORBIDDEN"
	case errors.Is(err, domain.ErrNotFound):
		return "NOT_FOUND"
	case errors.Is(err, domain.ErrConflict):
		return "CONFLICT"
	case errors.Is(err, domain.ErrInvalid):
		return "BAD_USER_INPUT"
	}
	return "INTERNAL"
}

func newGraphQL(svc *service.Service) http.Handler {
	srv := handler.New(graph.NewExecutableSchema(graph.Config{Resolvers: &graph.Resolver{Svc: svc}}))
	srv.AddTransport(transport.GET{})
	srv.AddTransport(transport.POST{})
	srv.Use(extension.Introspection{})
	// Bound query cost: nested model -> versions -> ... lists are cheap
	// individually but unbounded queries are an easy DoS.
	srv.Use(extension.FixedComplexityLimit(500))
	srv.SetErrorPresenter(func(ctx context.Context, err error) *gqlerror.Error {
		gerr := graphql.DefaultErrorPresenter(ctx, err)
		code := errorCode(err)
		if code == "INTERNAL" {
			var ge *gqlerror.Error
			// Parser/validation errors come through as gqlerror without a
			// wrapped domain error; keep their message.
			if errors.As(err, &ge) && ge.Unwrap() == nil {
				return gerr
			}
			gerr.Message = "internal error"
		}
		if gerr.Extensions == nil {
			gerr.Extensions = map[string]any{}
		}
		gerr.Extensions["code"] = code
		return gerr
	})
	return srv
}

func playgroundHandler() http.Handler {
	return playground.Handler("ModelDock GraphQL", "/graphql")
}
