package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

type ModelRole string

const (
	RolePlanner  ModelRole = "planner"
	RoleExecutor ModelRole = "executor"
	RoleFast     ModelRole = "fast"
)

type ModelProfile struct {
	Model         string `json:"model"`
	FallbackModel string `json:"fallback_model,omitempty"`
}

type ModelRoles struct {
	Planner  string `json:"planner,omitempty"`
	Executor string `json:"executor,omitempty"`
	Fast     string `json:"fast,omitempty"`
}

type ModelRouting struct {
	ModelProfiles map[string]ModelProfile `json:"model_profiles,omitempty"`
	ModelRoles    ModelRoles              `json:"model_roles,omitzero"`
}

// Profile returns the named profile for the role. Unmapped roles use the
// caller's existing default model and provider behavior.
func (r ModelRouting) Profile(role ModelRole) (string, ModelProfile, bool) {
	var name string
	switch role {
	case RolePlanner:
		name = r.ModelRoles.Planner
	case RoleExecutor:
		name = r.ModelRoles.Executor
	case RoleFast:
		name = r.ModelRoles.Fast
	}
	p, ok := r.ModelProfiles[name]
	return name, p, ok && name != ""
}

// LoadModelRouting reads just the routing settings from the process config
// sources. It shares the file decoder and environment precedence with Load.
func LoadModelRouting() (ModelRouting, error) {
	var cfg Config
	if err := loadFile("", os.Getenv, &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No config file on disk: routing is simply unconfigured.
			// A malformed file or bad routing env JSON still errors below.
			return ModelRouting{}, nil
		}
		return ModelRouting{}, err
	}
	if err := cfg.ModelRouting.applyEnv(os.Getenv); err != nil {
		return ModelRouting{}, err
	}
	return cfg.ModelRouting, nil
}

func (r *ModelRouting) applyEnv(env func(string) string) error {
	if raw := env("CLOUD_HARNESS_MODEL_PROFILES"); raw != "" {
		r.ModelProfiles = nil
		if err := decodeRoutingEnv("CLOUD_HARNESS_MODEL_PROFILES", raw, &r.ModelProfiles); err != nil {
			return err
		}
	}
	if raw := env("CLOUD_HARNESS_MODEL_ROLES"); raw != "" {
		r.ModelRoles = ModelRoles{}
		if err := decodeRoutingEnv("CLOUD_HARNESS_MODEL_ROLES", raw, &r.ModelRoles); err != nil {
			return err
		}
	}
	return r.validate()
}

func decodeRoutingEnv(name, raw string, dst any) error {
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return domain.E(domain.CodeInvalidArgument, name+": "+err.Error())
	}
	if err := d.Decode(new(json.RawMessage)); err != io.EOF {
		return domain.E(domain.CodeInvalidArgument, name+" must contain a single JSON object")
	}
	return nil
}

func (r ModelRouting) validate() error {
	names := make([]string, 0, len(r.ModelProfiles))
	for name := range r.ModelProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := r.ModelProfiles[name]
		if name == "" || strings.TrimSpace(name) != name {
			return domain.E(domain.CodeInvalidArgument, "model_profiles names must be non-empty and have no surrounding whitespace")
		}
		if p.Model == "" || strings.TrimSpace(p.Model) != p.Model {
			return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("model_profiles.%s.model must be non-empty and have no surrounding whitespace", name))
		}
		if strings.TrimSpace(p.FallbackModel) != p.FallbackModel || p.FallbackModel == p.Model {
			return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("model_profiles.%s.fallback_model must be different from model and have no surrounding whitespace", name))
		}
	}
	for _, role := range []ModelRole{RolePlanner, RoleExecutor, RoleFast} {
		if name, _, ok := r.Profile(role); name != "" && !ok {
			return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("model_roles.%s refers to unknown profile %q", role, name))
		}
	}
	return nil
}
