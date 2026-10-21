// Package api exposes the service over REST (/api/v1) and GraphQL
// (/graphql). Both call the same service methods, so authorization and
// state-machine rules are identical across transports.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wenxuewu0216/modeldock/auth"
	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/service"
)

const maxBody = 1 << 20

type rest struct {
	svc *service.Service
	log *slog.Logger
}

// statusFor maps domain errors to HTTP status codes.
func statusFor(err error) int {
	switch {
	case errors.Is(err, domain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, context.Canceled):
		return 499
	}
	return http.StatusInternalServerError
}

type errorBody struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders err as JSON. 5xx details are logged, not returned.
func writeError(log *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		code := statusFor(err)
		msg := err.Error()
		if code >= 500 {
			log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
			msg = "internal error"
		}
		writeJSON(w, code, errorBody{Error: msg, Code: code})
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: invalid JSON body: %v", domain.ErrInvalid, err)
	}
	return nil
}

// handler adapts a (user, request) -> (status, body, error) function.
type handlerFunc func(u domain.User, r *http.Request) (int, any, error)

func (h *rest) wrap(fn handlerFunc) http.HandlerFunc {
	onErr := writeError(h.log)
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := auth.CurrentUser(r.Context())
		if err != nil {
			onErr(w, r, err)
			return
		}
		code, body, err := fn(u, r)
		if err != nil {
			onErr(w, r, err)
			return
		}
		writeJSON(w, code, body)
	}
}

func (h *rest) routes(mux *routeMux) {
	mux.handle("GET /api/v1/me", "me", h.wrap(func(u domain.User, _ *http.Request) (int, any, error) {
		return 200, u, nil
	}))
	mux.handle("GET /api/v1/models", "list_models", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		ms, err := h.svc.ListModels(r.Context(), u)
		return 200, ms, err
	}))
	mux.handle("POST /api/v1/models", "create_model", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		var in struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		m, err := h.svc.CreateModel(r.Context(), u, in.Name, in.Description)
		return 201, m, err
	}))
	mux.handle("GET /api/v1/models/{model}", "get_model", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		m, err := h.svc.GetModel(r.Context(), u, r.PathValue("model"))
		return 200, m, err
	}))
	mux.handle("GET /api/v1/models/{model}/status", "model_status", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		st, err := h.svc.Status(r.Context(), u, r.PathValue("model"))
		return 200, st, err
	}))
	mux.handle("GET /api/v1/models/{model}/versions", "list_versions", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		vs, err := h.svc.ListVersions(r.Context(), u, r.PathValue("model"))
		return 200, vs, err
	}))
	mux.handle("POST /api/v1/models/{model}/versions", "register_version", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		var in domain.ModelVersion
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		v, err := h.svc.RegisterVersion(r.Context(), u, r.PathValue("model"), domain.ModelVersion{
			Version: in.Version, Image: in.Image, Port: in.Port, Metadata: in.Metadata,
			Resources: in.Resources, Scaling: in.Scaling,
		})
		return 201, v, err
	}))
	mux.handle("GET /api/v1/models/{model}/versions/{version}", "get_version", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		v, err := h.svc.GetVersion(r.Context(), u, r.PathValue("model"), r.PathValue("version"))
		return 200, v, err
	}))
	mux.handle("POST /api/v1/models/{model}/versions/{version}/deploy", "deploy_staging", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		var in struct {
			Environment string `json:"environment"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		if in.Environment != "" && in.Environment != string(domain.EnvStaging) {
			return 0, nil, fmt.Errorf("%w: only staging can be deployed directly; use /promotion-requests and /promote for prod", domain.ErrInvalid)
		}
		d, err := h.svc.DeployToStaging(r.Context(), u, r.PathValue("model"), r.PathValue("version"))
		return 202, d, err
	}))
	mux.handle("POST /api/v1/models/{model}/versions/{version}/promotion-requests", "request_promotion", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		var in struct {
			Comment string `json:"comment"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		a, err := h.svc.RequestPromotion(r.Context(), u, r.PathValue("model"), r.PathValue("version"), in.Comment)
		return 201, a, err
	}))
	mux.handle("POST /api/v1/models/{model}/versions/{version}/promote", "promote", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		d, err := h.svc.Promote(r.Context(), u, r.PathValue("model"), r.PathValue("version"))
		return 202, d, err
	}))
	mux.handle("POST /api/v1/models/{model}/rollback", "rollback", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		var in struct {
			Environment string `json:"environment"`
			ToVersion   string `json:"toVersion"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		env, err := domain.ParseEnvironment(in.Environment)
		if err != nil {
			return 0, nil, err
		}
		d, err := h.svc.Rollback(r.Context(), u, r.PathValue("model"), env, in.ToVersion)
		return 202, d, err
	}))
	mux.handle("GET /api/v1/models/{model}/deployments", "history", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		var env domain.Environment
		if e := r.URL.Query().Get("environment"); e != "" {
			var err error
			if env, err = domain.ParseEnvironment(e); err != nil {
				return 0, nil, err
			}
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		ds, err := h.svc.History(r.Context(), u, r.PathValue("model"), env, limit)
		return 200, ds, err
	}))
	mux.handle("GET /api/v1/models/{model}/diff", "diff", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		q := r.URL.Query()
		if q.Get("from") == "" || q.Get("to") == "" {
			return 0, nil, fmt.Errorf("%w: from and to query parameters are required", domain.ErrInvalid)
		}
		d, err := h.svc.Diff(r.Context(), u, r.PathValue("model"), q.Get("from"), q.Get("to"))
		return 200, d, err
	}))
	mux.handle("GET /api/v1/deployments/{id}", "get_deployment", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		d, err := h.svc.GetDeployment(r.Context(), u, r.PathValue("id"))
		return 200, d, err
	}))
	mux.handle("GET /api/v1/approvals", "list_approvals", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		as, err := h.svc.ListApprovals(r.Context(), u, domain.ApprovalStatus(r.URL.Query().Get("status")))
		return 200, as, err
	}))
	mux.handle("GET /api/v1/approvals/{id}", "get_approval", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		a, err := h.svc.GetApproval(r.Context(), u, r.PathValue("id"))
		return 200, a, err
	}))
	decide := func(approve bool) handlerFunc {
		return func(u domain.User, r *http.Request) (int, any, error) {
			var in struct {
				Comment string `json:"comment"`
			}
			if err := decode(r, &in); err != nil {
				return 0, nil, err
			}
			a, err := h.svc.Decide(r.Context(), u, r.PathValue("id"), approve, in.Comment)
			return 200, a, err
		}
	}
	mux.handle("POST /api/v1/approvals/{id}/approve", "approve", h.wrap(decide(true)))
	mux.handle("POST /api/v1/approvals/{id}/reject", "reject", h.wrap(decide(false)))
	mux.handle("GET /api/v1/audit", "audit", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		q := r.URL.Query()
		before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		evs, err := h.svc.Audit(r.Context(), u, q.Get("model"), before, limit)
		return 200, evs, err
	}))
	mux.handle("GET /api/v1/dashboard", "dashboard", h.wrap(func(u domain.User, r *http.Request) (int, any, error) {
		d, err := h.svc.Dashboard(r.Context(), u)
		return 200, d, err
	}))
}
