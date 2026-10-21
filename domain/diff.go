package domain

import (
	"fmt"
	"sort"
)

// FieldChange is one difference between two model versions.
type FieldChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// VersionDiff describes what changes when moving from one version to another.
type VersionDiff struct {
	Model   string        `json:"model"`
	From    string        `json:"from"`
	To      string        `json:"to"`
	Changes []FieldChange `json:"changes"`
}

// Diff compares the deployable configuration of two versions of the same
// model. Fields are reported in a stable order; metadata keys are sorted.
func Diff(a, b ModelVersion) VersionDiff {
	d := VersionDiff{Model: a.ModelName, From: a.Version, To: b.Version, Changes: []FieldChange{}}
	add := func(field, from, to string) {
		if from != to {
			d.Changes = append(d.Changes, FieldChange{Field: field, From: from, To: to})
		}
	}
	itoa := func(i int32) string { return fmt.Sprintf("%d", i) }

	add("image", a.Image, b.Image)
	add("port", itoa(a.Port), itoa(b.Port))
	add("resources.cpuRequest", a.Resources.CPURequest, b.Resources.CPURequest)
	add("resources.memoryRequest", a.Resources.MemoryRequest, b.Resources.MemoryRequest)
	add("resources.cpuLimit", a.Resources.CPULimit, b.Resources.CPULimit)
	add("resources.memoryLimit", a.Resources.MemoryLimit, b.Resources.MemoryLimit)
	add("scaling.minReplicas", itoa(a.Scaling.MinReplicas), itoa(b.Scaling.MinReplicas))
	add("scaling.maxReplicas", itoa(a.Scaling.MaxReplicas), itoa(b.Scaling.MaxReplicas))
	add("scaling.targetCPUPercentage", itoa(a.Scaling.TargetCPUPercentage), itoa(b.Scaling.TargetCPUPercentage))

	keys := map[string]struct{}{}
	for k := range a.Metadata {
		keys[k] = struct{}{}
	}
	for k := range b.Metadata {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		add("metadata."+k, a.Metadata[k], b.Metadata[k])
	}
	return d
}
