package policy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/yaml"
)

type Mode string

const (
	ModeEnforce Mode = "enforce"
	ModeAudit   Mode = "audit"
)

type Config struct {
	Mode      Mode            `json:"mode"`
	Agents    AgentConfig     `json:"agents"`
	Dedupe    DedupeConfig    `json:"dedupe"`
	Ownership OwnershipConfig `json:"ownership"`
	Limits    LimitsConfig    `json:"limits"`
}

// AgentConfig decides which requesters are treated as agents. Service accounts
// are written as "namespace/name"; "namespace/*" matches every account in it.
type AgentConfig struct {
	ServiceAccounts []string `json:"serviceAccounts"`
	Users           []string `json:"users"`
	Groups          []string `json:"groups"`
}

type DedupeConfig struct {
	Window Duration `json:"window"`
}

type OwnershipConfig struct {
	ProtectedManagers  []ManagerMatch `json:"protectedManagers"`
	OverrideAnnotation string         `json:"overrideAnnotation"`
}

// ManagerMatch selects managedFields entries. Manager supports a trailing "*"
// wildcard; an empty Subresource matches any subresource.
type ManagerMatch struct {
	Manager     string `json:"manager"`
	Subresource string `json:"subresource,omitempty"`
}

type LimitsConfig struct {
	Default    Limits            `json:"default"`
	Namespaces map[string]Limits `json:"namespaces"`
}

// Limits caps the blast radius of agent writes. Zero values mean unlimited.
type Limits struct {
	MaxDeletes      int      `json:"maxDeletes,omitempty"`
	Window          Duration `json:"window,omitempty"`
	MaxReplicaDelta int      `json:"maxReplicaDelta,omitempty"`
}

type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func DefaultConfig() Config {
	return Config{
		Mode:   ModeEnforce,
		Dedupe: DedupeConfig{Window: Duration{10 * time.Minute}},
		Ownership: OwnershipConfig{
			OverrideAnnotation: AnnotationOwnershipOverride,
			ProtectedManagers: []ManagerMatch{
				{Manager: "kube-controller-manager", Subresource: "scale"},
				{Manager: "argocd-controller"},
				{Manager: "argocd-application-controller"},
				{Manager: "kustomize-controller"},
				{Manager: "helm-controller"},
			},
		},
		Limits: LimitsConfig{Default: Limits{Window: Duration{10 * time.Minute}}},
	}
}

// ParseConfig reads YAML or JSON and layers it over DefaultConfig.
func ParseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(string(data)) == "" {
		return cfg, nil
	}
	raw, err := yaml.ToJSON(data)
	if err != nil {
		return Config{}, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if c.Mode != ModeEnforce && c.Mode != ModeAudit {
		return fmt.Errorf("mode must be %q or %q, got %q", ModeEnforce, ModeAudit, c.Mode)
	}
	if c.Dedupe.Window.Duration < 0 {
		return fmt.Errorf("dedupe.window must not be negative")
	}
	for _, sa := range c.Agents.ServiceAccounts {
		if ns, name, ok := strings.Cut(sa, "/"); !ok || ns == "" || name == "" {
			return fmt.Errorf("agents.serviceAccounts entry %q must be namespace/name", sa)
		}
	}
	for ns, l := range c.Limits.Namespaces {
		if l.MaxDeletes > 0 && c.limitsFor(ns).Window.Duration <= 0 {
			return fmt.Errorf("limits.namespaces.%s: maxDeletes needs a window", ns)
		}
	}
	if c.Limits.Default.MaxDeletes > 0 && c.Limits.Default.Window.Duration <= 0 {
		return fmt.Errorf("limits.default: maxDeletes needs a window")
	}
	return nil
}

// limitsFor merges namespace overrides onto the defaults field by field.
func (c Config) limitsFor(ns string) Limits {
	l := c.Limits.Default
	o, ok := c.Limits.Namespaces[ns]
	if !ok {
		return l
	}
	if o.MaxDeletes != 0 {
		l.MaxDeletes = o.MaxDeletes
	}
	if o.Window.Duration != 0 {
		l.Window = o.Window
	}
	if o.MaxReplicaDelta != 0 {
		l.MaxReplicaDelta = o.MaxReplicaDelta
	}
	return l
}
