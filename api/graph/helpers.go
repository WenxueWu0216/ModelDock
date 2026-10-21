package graph

import (
	"context"
	"errors"
	"sort"

	"github.com/wenxuewu0216/modeldock/auth"
	"github.com/wenxuewu0216/modeldock/domain"
)

func user(ctx context.Context) (domain.User, error) { return auth.CurrentUser(ctx) }

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func i32(p *int) int32 {
	if p == nil {
		return 0
	}
	return int32(*p)
}

// nilIfNotFound turns ErrNotFound into a null result, the GraphQL idiom for
// single-object lookups.
func nilIfNotFound[T any](v T, err error) (*T, error) {
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func keyValues(m map[string]string) []KeyValue {
	out := make([]KeyValue, 0, len(m))
	for k, v := range m {
		out = append(out, KeyValue{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func toVersion(in RegisterVersionInput) domain.ModelVersion {
	v := domain.ModelVersion{Version: in.Version, Image: in.Image, Metadata: map[string]string{}}
	if in.Port != nil {
		v.Port = int32(*in.Port)
	}
	for _, kv := range in.Metadata {
		v.Metadata[kv.Key] = kv.Value
	}
	if r := in.Resources; r != nil {
		v.Resources = domain.Resources{CPURequest: str(r.CPURequest), MemoryRequest: str(r.MemoryRequest),
			CPULimit: str(r.CPULimit), MemoryLimit: str(r.MemoryLimit)}
	}
	if s := in.Scaling; s != nil {
		v.Scaling = domain.Scaling{MinReplicas: i32(s.MinReplicas), MaxReplicas: i32(s.MaxReplicas),
			TargetCPUPercentage: i32(s.TargetCPUPercentage)}
	}
	return v
}
