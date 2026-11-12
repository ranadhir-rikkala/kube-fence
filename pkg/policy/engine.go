package policy

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/ranadhir-rikkala/kube-fence/pkg/ledger"
)

type Engine struct {
	cfg    Config
	ledger ledger.Ledger
	now    func() time.Time
	mu     sync.Mutex
}

func NewEngine(cfg Config, l ledger.Ledger, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{cfg: cfg, ledger: l, now: now}
}

func (e *Engine) Config() Config { return e.cfg }

func (e *Engine) IsAgent(u UserInfo) bool { return e.cfg.Agents.IsAgent(u) }

// Evaluate decides on a single admission request and, when the write is
// allowed (or the engine is in audit mode), records it in the ledger.
func (e *Engine) Evaluate(req Request) Decision {
	d := Decision{Allowed: true}
	if req.Operation == OpConnect || !e.cfg.Agents.IsAgent(req.User) {
		return d
	}
	d.Agent = true

	var changes []Change
	switch req.Operation {
	case OpCreate:
		changes = Diff(nil, req.Object)
	case OpUpdate:
		changes = Diff(req.OldObject, req.Object)
		if len(changes) == 0 {
			d.NoOp = true
			return d
		}
	}
	d.IntentKey = IntentKey(req, changes)

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()

	d.Violations = append(d.Violations, e.checkDuplicate(req, d.IntentKey, now)...)
	if req.Operation == OpUpdate {
		d.Violations = append(d.Violations, e.checkOwnership(req, changes)...)
	}
	d.Violations = append(d.Violations, e.checkLimits(req, now)...)

	if len(d.Violations) > 0 {
		if e.cfg.Mode != ModeAudit {
			d.Allowed = false
			return d
		}
		d.Audit = true
	}
	if req.DryRun {
		return d
	}
	err := e.ledger.Record(ledger.Entry{
		Key:       d.IntentKey,
		Agent:     req.User.Username,
		Operation: string(req.Operation),
		Resource:  req.Resource.String(),
		Namespace: req.Namespace,
		Name:      req.Name,
		At:        now,
	})
	if err != nil && e.cfg.Mode != ModeAudit {
		d.Allowed = false
		d.Violations = append(d.Violations, Violation{Rule: "ledger", Message: "could not record effect: " + err.Error()})
		return d
	}
	d.Recorded = err == nil
	return d
}

func (e *Engine) checkDuplicate(req Request, key string, now time.Time) []Violation {
	w := e.cfg.Dedupe.Window.Duration
	if w <= 0 {
		return nil
	}
	prev, ok := e.ledger.Lookup(key, now.Add(-w))
	if !ok {
		return nil
	}
	msg := fmt.Sprintf("duplicate irreversible effect: %s on %s matches intent %s already applied by %s %s ago; if this is a new action, set a fresh %s annotation",
		req.Operation, req.target(), key, prev.Agent, now.Sub(prev.At).Round(time.Second), AnnotationIntentID)
	return []Violation{{Rule: RuleDuplicate, Message: msg}}
}

func (e *Engine) checkOwnership(req Request, changes []Change) []Violation {
	oc := e.cfg.Ownership
	if len(oc.ProtectedManagers) == 0 || overrideRequested(req, oc.OverrideAnnotation) {
		return nil
	}
	conflicts := ownershipConflicts(oc.ProtectedManagers, req.OldObject, changes)
	if len(conflicts) == 0 {
		return nil
	}
	parts := make([]string, len(conflicts))
	for i, c := range conflicts {
		owner := c.Manager
		if c.Subresource != "" {
			owner += " via /" + c.Subresource
		}
		parts[i] = fmt.Sprintf("%s (owned by %s)", c.Path, owner)
	}
	msg := fmt.Sprintf("write to %s changes fields managed by another controller: %s; the controller would revert or fight this change. Set %s on this request to override",
		req.target(), strings.Join(parts, ", "), oc.OverrideAnnotation)
	return []Violation{{Rule: RuleOwnership, Message: msg}}
}

// overrideRequested only honours an override that this request sets or
// changes, so an annotation left on the object cannot silence later writes.
func overrideRequested(req Request, key string) bool {
	if key == "" {
		return false
	}
	v := annotation(req.Object, key)
	return v != "" && v != "false" && v != annotation(req.OldObject, key)
}

func (e *Engine) checkLimits(req Request, now time.Time) []Violation {
	l := e.cfg.limitsFor(req.Namespace)
	var out []Violation
	if req.Operation == OpDelete && l.MaxDeletes > 0 {
		n := e.ledger.Count(ledger.Filter{Namespace: req.Namespace, Operation: string(OpDelete)}, now.Add(-l.Window.Duration))
		if n >= l.MaxDeletes {
			out = append(out, Violation{Rule: RuleDeleteLimit, Message: fmt.Sprintf(
				"agents already deleted %d object(s) in namespace %q within %s (limit %d)", n, req.Namespace, l.Window.Duration, l.MaxDeletes)})
		}
	}
	if req.Operation == OpUpdate && l.MaxReplicaDelta > 0 {
		before, okOld := replicas(req.OldObject)
		after, okNew := replicas(req.Object)
		if okOld && okNew {
			if delta := after - before; abs(delta) > int64(l.MaxReplicaDelta) {
				out = append(out, Violation{Rule: RuleReplicaCap, Message: fmt.Sprintf(
					"replica change %d -> %d on %s exceeds max delta %d for namespace %q", before, after, req.target(), l.MaxReplicaDelta, req.Namespace)})
			}
		}
	}
	return out
}

func replicas(obj map[string]any) (int64, bool) {
	spec, _ := obj["spec"].(map[string]any)
	switch v := spec["replicas"].(type) {
	case float64:
		if v == math.Trunc(v) {
			return int64(v), true
		}
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
