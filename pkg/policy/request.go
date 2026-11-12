// Package policy decides whether an agent write may proceed. It performs no
// I/O: callers supply the request, a ledger, and a clock.
package policy

import (
	"fmt"
	"strings"
)

const (
	AnnotationPrefix            = "kube-fence.io/"
	AnnotationIntentID          = AnnotationPrefix + "intent-id"
	AnnotationOwnershipOverride = AnnotationPrefix + "allow-ownership-override"
	AnnotationLastAgentWrite    = AnnotationPrefix + "last-agent-write"
)

type Operation string

const (
	OpCreate  Operation = "CREATE"
	OpUpdate  Operation = "UPDATE"
	OpDelete  Operation = "DELETE"
	OpConnect Operation = "CONNECT"
)

type GVR struct {
	Group    string
	Version  string
	Resource string
}

func (g GVR) String() string {
	if g.Group == "" {
		return g.Version + "/" + g.Resource
	}
	return g.Group + "/" + g.Version + "/" + g.Resource
}

type UserInfo struct {
	Username string
	Groups   []string
}

// Request is the admission request reduced to what the policy needs. Object
// and OldObject are decoded JSON (map[string]any).
type Request struct {
	UID         string
	Operation   Operation
	User        UserInfo
	Resource    GVR
	SubResource string
	Namespace   string
	Name        string
	Object      map[string]any
	OldObject   map[string]any
	DryRun      bool
}

func (r Request) target() string {
	res := r.Resource.Resource
	if r.SubResource != "" {
		res += "/" + r.SubResource
	}
	name := r.Name
	if name == "" {
		name = "<generated>"
	}
	if r.Namespace == "" {
		return fmt.Sprintf("%s %s", res, name)
	}
	return fmt.Sprintf("%s %s/%s", res, r.Namespace, name)
}

type Rule string

const (
	RuleDuplicate   Rule = "duplicate-effect"
	RuleOwnership   Rule = "field-ownership"
	RuleDeleteLimit Rule = "delete-limit"
	RuleReplicaCap  Rule = "replica-delta"
)

type Violation struct {
	Rule    Rule   `json:"rule"`
	Message string `json:"message"`
}

type Decision struct {
	Allowed    bool        `json:"allowed"`
	Agent      bool        `json:"agent"`
	Audit      bool        `json:"audit,omitempty"`
	Recorded   bool        `json:"recorded"`
	NoOp       bool        `json:"noOp,omitempty"`
	IntentKey  string      `json:"intentKey,omitempty"`
	Violations []Violation `json:"violations,omitempty"`
}

func (d Decision) Message() string {
	if len(d.Violations) == 0 {
		return ""
	}
	parts := make([]string, len(d.Violations))
	for i, v := range d.Violations {
		parts[i] = fmt.Sprintf("[%s] %s", v.Rule, v.Message)
	}
	prefix := "kube-fence denied agent write: "
	if d.Audit {
		prefix = "kube-fence (audit) would deny agent write: "
	}
	return prefix + strings.Join(parts, "; ")
}

const saPrefix = "system:serviceaccount:"

// IsAgent reports whether the requester matches the agent identity config.
func (c AgentConfig) IsAgent(u UserInfo) bool {
	for _, name := range c.Users {
		if name == u.Username {
			return true
		}
	}
	for _, g := range u.Groups {
		for _, want := range c.Groups {
			if g == want {
				return true
			}
		}
	}
	rest, ok := strings.CutPrefix(u.Username, saPrefix)
	if !ok {
		return false
	}
	ns, name, ok := strings.Cut(rest, ":")
	if !ok {
		return false
	}
	for _, sa := range c.ServiceAccounts {
		wantNS, wantName, _ := strings.Cut(sa, "/")
		if wantNS == ns && (wantName == "*" || wantName == name) {
			return true
		}
	}
	return false
}
