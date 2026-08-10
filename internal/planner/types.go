package planner

import (
	"context"
	"encoding/json"
)

const SupportedSchemaVersion = "1.0"

// Runner produces a deployment plan for a source tree.
type Runner interface {
	Plan(ctx context.Context, sourceRoot string) (*Result, error)
}

type Result struct {
	Pods PodManifest
	Mesh json.RawMessage
}

type PodManifest struct {
	Version     string          `json:"version"`
	Deployments []PodDeployment `json:"deployments"`
	Edges       []Edge          `json:"edges"`
	Metrics     GlobalMetrics   `json:"metrics"`
}

type PodDeployment struct {
	ID           int          `json:"id"`
	Language     string       `json:"language"`
	Files        []string     `json:"files"`
	Metrics      PodMetrics   `json:"metrics"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

type PodMetrics struct {
	TotalASTNodes int `json:"total_ast_nodes"`
	FileCount     int `json:"file_count"`
	SymbolCount   int `json:"symbol_count"`
}

type Dependency struct {
	Name     string  `json:"name"`
	Version  *string `json:"version"`
	Language string  `json:"language"`
	Source   string  `json:"source"`
}

type Edge struct {
	FromPod       int             `json:"from_pod"`
	ToPod         int             `json:"to_pod"`
	Kind          string          `json:"kind"`
	Confidence    float64         `json:"confidence"`
	CrossLanguage bool            `json:"is_cross_language"`
	CutAnnotation json.RawMessage `json:"cut_annotation,omitempty"`
}

type GlobalMetrics struct {
	TotalPods          int `json:"total_pods"`
	CrossLanguageEdges int `json:"cross_language_edges"`
	TotalASTNodes      int `json:"total_ast_nodes"`
}
