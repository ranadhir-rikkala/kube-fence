package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ranadhir-rikkala/kube-fence/pkg/ledger"
)

var (
	t0       = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	agent    = UserInfo{Username: "system:serviceaccount:agents:remediator", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:agents"}}
	human    = UserInfo{Username: "alice@example.com", Groups: []string{"system:authenticated"}}
	deployGV = GVR{Group: "apps", Version: "v1", Resource: "deployments"}
	podGV    = GVR{Version: "v1", Resource: "pods"}
	jobGV    = GVR{Group: "batch", Version: "v1", Resource: "jobs"}
)

func obj(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return m
}

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Agents.ServiceAccounts = []string{"agents/*"}
	cfg.Limits.Default = Limits{MaxDeletes: 2, Window: Duration{10 * time.Minute}, MaxReplicaDelta: 3}
	cfg.Limits.Namespaces = map[string]Limits{"prod": {MaxDeletes: 1, MaxReplicaDelta: 1}}
	return cfg
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

const hpaManaged = `"managedFields": [
  {"manager": "kubectl-client-side-apply", "operation": "Update", "fieldsV1": {"f:spec": {"f:template": {"f:spec": {"f:containers": {"k:{\"name\":\"app\"}": {".": {}, "f:image": {}}}}}}}},
  {"manager": "kube-controller-manager", "operation": "Update", "subresource": "scale", "fieldsV1": {"f:spec": {"f:replicas": {}}}}
]`

const argoManaged = `"managedFields": [
  {"manager": "argocd-controller", "operation": "Apply", "fieldsV1": {"f:metadata": {"f:labels": {"f:app": {}}}, "f:spec": {"f:template": {"f:spec": {"f:containers": {"k:{\"name\":\"app\"}": {".": {}, "f:image": {}, "f:name": {}}}}}}}}
]`

func deployment(t *testing.T, managed string, replicas int, image string, annotations string) map[string]any {
	t.Helper()
	if annotations == "" {
		annotations = "{}"
	}
	return obj(t, `{
  "apiVersion": "apps/v1", "kind": "Deployment",
  "metadata": {"name": "web", "namespace": "shop", "resourceVersion": "1", "annotations": `+annotations+`, `+managed+`},
  "spec": {"replicas": `+itoa(replicas)+`, "template": {"spec": {"containers": [{"name": "app", "image": "`+image+`"}]}}},
  "status": {"replicas": 3}
}`)
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

type step struct {
	name      string
	at        time.Duration
	req       Request
	allowed   bool
	rules     []Rule
	recorded  bool
	noOp      bool
	audit     bool
	msgSubstr string
}

func runSteps(t *testing.T, cfg Config, steps []step) {
	t.Helper()
	c := &clock{t: t0}
	e := NewEngine(cfg, ledger.NewMemory(time.Hour), c.now)
	for _, s := range steps {
		c.t = t0.Add(s.at)
		d := e.Evaluate(s.req)
		if d.Allowed != s.allowed {
			t.Fatalf("%s: allowed = %v, want %v (%s)", s.name, d.Allowed, s.allowed, d.Message())
		}
		if got := rulesOf(d); strings.Join(got, ",") != joinRules(s.rules) {
			t.Fatalf("%s: rules = %v, want %v", s.name, got, s.rules)
		}
		if d.Recorded != s.recorded {
			t.Fatalf("%s: recorded = %v, want %v", s.name, d.Recorded, s.recorded)
		}
		if d.NoOp != s.noOp {
			t.Fatalf("%s: noOp = %v, want %v", s.name, d.NoOp, s.noOp)
		}
		if d.Audit != s.audit {
			t.Fatalf("%s: audit = %v, want %v", s.name, d.Audit, s.audit)
		}
		if s.msgSubstr != "" && !strings.Contains(d.Message(), s.msgSubstr) {
			t.Fatalf("%s: message %q does not contain %q", s.name, d.Message(), s.msgSubstr)
		}
	}
}

func rulesOf(d Decision) []string {
	var out []string
	for _, v := range d.Violations {
		out = append(out, string(v.Rule))
	}
	return out
}

func joinRules(rs []Rule) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = string(r)
	}
	return strings.Join(s, ",")
}

func TestEvaluate(t *testing.T) {
	podDelete := func(u UserInfo, ns, name string) Request {
		return Request{Operation: OpDelete, User: u, Resource: podGV, Namespace: ns, Name: name,
			OldObject: obj(t, `{"metadata": {"name": "`+name+`", "uid": "u-1"}}`)}
	}
	update := func(u UserInfo, old, new map[string]any) Request {
		return Request{Operation: OpUpdate, User: u, Resource: deployGV, Namespace: "shop", Name: "web", OldObject: old, Object: new}
	}
	jobCreate := func(intent, generatedName string) Request {
		ann := "{}"
		if intent != "" {
			ann = `{"` + AnnotationIntentID + `": "` + intent + `"}`
		}
		return Request{Operation: OpCreate, User: agent, Resource: jobGV, Namespace: "shop", Name: generatedName,
			Object: obj(t, `{"metadata": {"name": "`+generatedName+`", "generateName": "migrate-", "annotations": `+ann+`}, "spec": {"template": {"spec": {"containers": [{"name": "m", "image": "migrate:v7"}]}}}}`)}
	}
	plain := deployment(t, `"managedFields": []`, 3, "app:v1", "")

	tests := []struct {
		name  string
		cfg   func(*Config)
		steps []step
	}{
		{
			name: "non-agent writes bypass every rule",
			steps: []step{
				{name: "human delete", req: podDelete(human, "prod", "web-0"), allowed: true},
				{name: "human delete again", req: podDelete(human, "prod", "web-0"), allowed: true},
				{name: "human scales past cap", req: update(human, plain, deployment(t, `"managedFields": []`, 30, "app:v1", "")), allowed: true},
			},
		},
		{
			name: "retried statefulset pod delete is a duplicate effect",
			steps: []step{
				{name: "first delete", req: podDelete(agent, "shop", "db-0"), allowed: true, recorded: true},
				{name: "retry after pod was recreated", at: 30 * time.Second, req: podDelete(agent, "shop", "db-0"), rules: []Rule{RuleDuplicate}, msgSubstr: "duplicate irreversible effect"},
				{name: "same delete after window", at: 11 * time.Minute, req: podDelete(agent, "shop", "db-0"), allowed: true, recorded: true},
			},
		},
		{
			name: "a second agent repeating the same effect is also a duplicate",
			steps: []step{
				{name: "agent a", req: podDelete(agent, "shop", "db-0"), allowed: true, recorded: true},
				{name: "agent b", req: podDelete(UserInfo{Username: "system:serviceaccount:agents:triage"}, "shop", "db-0"), rules: []Rule{RuleDuplicate}},
			},
		},
		{
			name: "generateName creates are deduplicated by content",
			steps: []step{
				{name: "first job", req: jobCreate("", "migrate-x7k2p"), allowed: true, recorded: true},
				{name: "retried job gets a different random name", at: time.Minute, req: jobCreate("", "migrate-9qf4z"), rules: []Rule{RuleDuplicate}},
			},
		},
		{
			name: "explicit intent ids override content hashing",
			steps: []step{
				{name: "intent a", req: jobCreate("ticket-42", "migrate-aaaaa"), allowed: true, recorded: true},
				{name: "intent a again", req: jobCreate("ticket-42", "migrate-bbbbb"), rules: []Rule{RuleDuplicate}, msgSubstr: "intent:ticket-42"},
				{name: "intent b with same body", req: jobCreate("ticket-43", "migrate-ccccc"), allowed: true, recorded: true},
			},
		},
		{
			name: "relative recompute with the same intent id is caught",
			steps: []step{
				{name: "scale 3 to 5", req: update(agent, plain, deployment(t, `"managedFields": []`, 5, "app:v1", `{"kube-fence.io/intent-id": "scale-up-1"}`)), allowed: true, recorded: true},
				{name: "retry recomputes 5 to 7", req: update(agent, deployment(t, `"managedFields": []`, 5, "app:v1", `{"kube-fence.io/intent-id": "scale-up-1"}`), deployment(t, `"managedFields": []`, 7, "app:v1", `{"kube-fence.io/intent-id": "scale-up-1"}`)), rules: []Rule{RuleDuplicate}},
			},
		},
		{
			name: "no-op updates are allowed and not recorded",
			steps: []step{
				{name: "identical object", req: update(agent, plain, deployment(t, `"managedFields": []`, 3, "app:v1", "")), allowed: true, noOp: true},
				{name: "only kube-fence annotations differ", req: update(agent, plain, deployment(t, `"managedFields": []`, 3, "app:v1", `{"kube-fence.io/last-agent-write": "x"}`)), allowed: true, noOp: true},
			},
		},
		{
			name: "replicas owned by the HPA via the scale subresource",
			steps: []step{
				{name: "agent sets replicas", req: update(agent, deployment(t, hpaManaged, 3, "app:v1", ""), deployment(t, hpaManaged, 4, "app:v1", "")), rules: []Rule{RuleOwnership}, msgSubstr: "spec.replicas (owned by kube-controller-manager via /scale)"},
				{name: "agent changes image it owns", req: update(agent, deployment(t, hpaManaged, 3, "app:v1", ""), deployment(t, hpaManaged, 3, "app:v2", "")), allowed: true, recorded: true},
				{name: "override set by this request", req: update(agent, deployment(t, hpaManaged, 3, "app:v1", ""), deployment(t, hpaManaged, 4, "app:v1", `{"kube-fence.io/allow-ownership-override": "incident-881"}`)), allowed: true, recorded: true},
				{name: "override left over from an earlier write", req: update(agent, deployment(t, hpaManaged, 4, "app:v1", `{"kube-fence.io/allow-ownership-override": "incident-881"}`), deployment(t, hpaManaged, 5, "app:v1", `{"kube-fence.io/allow-ownership-override": "incident-881"}`)), rules: []Rule{RuleOwnership}},
			},
		},
		{
			name: "fields applied by argocd are protected",
			steps: []step{
				{name: "image bump", req: update(agent, deployment(t, argoManaged, 3, "app:v1", ""), deployment(t, argoManaged, 3, "app:v2", "")), rules: []Rule{RuleOwnership}, msgSubstr: "containers[name=app].image (owned by argocd-controller)"},
				{name: "replicas not in git", req: update(agent, deployment(t, argoManaged, 3, "app:v1", ""), deployment(t, argoManaged, 4, "app:v1", "")), allowed: true, recorded: true},
			},
		},
		{
			name: "ownership guard is disabled with no protected managers",
			cfg:  func(c *Config) { c.Ownership.ProtectedManagers = nil },
			steps: []step{
				{name: "image bump", req: update(agent, deployment(t, argoManaged, 3, "app:v1", ""), deployment(t, argoManaged, 3, "app:v2", "")), allowed: true, recorded: true},
			},
		},
		{
			name: "per-namespace delete cap",
			steps: []step{
				{name: "prod delete 1", req: podDelete(agent, "prod", "a"), allowed: true, recorded: true},
				{name: "prod delete 2", at: time.Minute, req: podDelete(agent, "prod", "b"), rules: []Rule{RuleDeleteLimit}, msgSubstr: "limit 1"},
				{name: "other namespace uses default cap", at: time.Minute, req: podDelete(agent, "shop", "b"), allowed: true, recorded: true},
				{name: "prod window expired", at: 11 * time.Minute, req: podDelete(agent, "prod", "c"), allowed: true, recorded: true},
			},
		},
		{
			name: "replica delta cap",
			steps: []step{
				{name: "small change", req: update(agent, plain, deployment(t, `"managedFields": []`, 6, "app:v1", "")), allowed: true, recorded: true},
				{name: "large jump", req: update(agent, plain, deployment(t, `"managedFields": []`, 10, "app:v1", "")), rules: []Rule{RuleReplicaCap}, msgSubstr: "3 -> 10"},
			},
		},
		{
			name: "audit mode allows but reports and records",
			cfg:  func(c *Config) { c.Mode = ModeAudit },
			steps: []step{
				{name: "first", req: podDelete(agent, "shop", "db-0"), allowed: true, recorded: true},
				{name: "duplicate", req: podDelete(agent, "shop", "db-0"), allowed: true, recorded: true, audit: true, rules: []Rule{RuleDuplicate}, msgSubstr: "(audit) would deny"},
			},
		},
		{
			name: "dry-run requests are evaluated but not recorded",
			steps: []step{
				{name: "dry run", req: func() Request { r := podDelete(agent, "shop", "db-0"); r.DryRun = true; return r }(), allowed: true},
				{name: "real delete is not a duplicate", req: podDelete(agent, "shop", "db-0"), allowed: true, recorded: true},
			},
		},
		{
			name: "multiple violations are reported together",
			steps: []step{
				{name: "seed", req: update(agent, deployment(t, hpaManaged, 3, "app:v1", ""), deployment(t, hpaManaged, 3, "app:v2", `{"kube-fence.io/intent-id": "x"}`)), allowed: true, recorded: true},
				{name: "dup + ownership + delta", req: update(agent, deployment(t, hpaManaged, 3, "app:v1", ""), deployment(t, hpaManaged, 10, "app:v1", `{"kube-fence.io/intent-id": "x"}`)), rules: []Rule{RuleDuplicate, RuleOwnership, RuleReplicaCap}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			runSteps(t, cfg, tt.steps)
		})
	}
}

type failingLedger struct{ ledger.Ledger }

func (failingLedger) Record(ledger.Entry) error { return errString("disk full") }

type errString string

func (e errString) Error() string { return string(e) }

func TestEvaluateFailsClosedWhenLedgerWriteFails(t *testing.T) {
	e := NewEngine(testConfig(), failingLedger{ledger.NewMemory(time.Hour)}, func() time.Time { return t0 })
	d := e.Evaluate(Request{Operation: OpDelete, User: agent, Resource: podGV, Namespace: "shop", Name: "x"})
	if d.Allowed || d.Recorded {
		t.Fatalf("decision = %+v, want denied and unrecorded", d)
	}
}
