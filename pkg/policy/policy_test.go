package policy

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestIsAgent(t *testing.T) {
	cfg := AgentConfig{
		ServiceAccounts: []string{"agents/*", "ops/remediator"},
		Users:           []string{"llm-operator@example.com"},
		Groups:          []string{"kube-fence:agents"},
	}
	tests := []struct {
		name string
		user UserInfo
		want bool
	}{
		{"wildcard namespace", UserInfo{Username: "system:serviceaccount:agents:anything"}, true},
		{"exact service account", UserInfo{Username: "system:serviceaccount:ops:remediator"}, true},
		{"other account in ops", UserInfo{Username: "system:serviceaccount:ops:deployer"}, false},
		{"prefix is not a namespace match", UserInfo{Username: "system:serviceaccount:agents-x:a"}, false},
		{"listed user", UserInfo{Username: "llm-operator@example.com"}, true},
		{"group member", UserInfo{Username: "bob", Groups: []string{"devs", "kube-fence:agents"}}, true},
		{"human", UserInfo{Username: "alice", Groups: []string{"system:authenticated"}}, false},
		{"malformed sa username", UserInfo{Username: "system:serviceaccount:agents"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.IsAgent(tt.user); got != tt.want {
				t.Fatalf("IsAgent(%+v) = %v, want %v", tt.user, got, tt.want)
			}
		})
	}
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
		want []string
	}{
		{
			name: "server-managed fields ignored",
			old:  `{"metadata": {"resourceVersion": "1", "generation": 1, "managedFields": []}, "status": {"a": 1}, "spec": {"x": 1}}`,
			new:  `{"metadata": {"resourceVersion": "2", "generation": 2}, "status": {"a": 2}, "spec": {"x": 1}}`,
			want: nil,
		},
		{
			name: "scalar change",
			old:  `{"spec": {"replicas": 3}}`,
			new:  `{"spec": {"replicas": 5}}`,
			want: []string{"spec.replicas"},
		},
		{
			name: "named list elements matched by name",
			old:  `{"spec": {"containers": [{"name": "a", "image": "a:1"}, {"name": "b", "image": "b:1"}]}}`,
			new:  `{"spec": {"containers": [{"name": "b", "image": "b:2"}, {"name": "a", "image": "a:1"}]}}`,
			want: []string{"spec.containers[name=b].image"},
		},
		{
			name: "added and removed list elements",
			old:  `{"spec": {"containers": [{"name": "a"}]}}`,
			new:  `{"spec": {"containers": [{"name": "c"}]}}`,
			want: []string{"spec.containers[name=a]", "spec.containers[name=c]"},
		},
		{
			name: "unkeyed list compared whole",
			old:  `{"spec": {"args": ["--a"]}}`,
			new:  `{"spec": {"args": ["--a", "--b"]}}`,
			want: []string{"spec.args"},
		},
		{
			name: "labels and foreign annotations count, kube-fence annotations do not",
			old:  `{"metadata": {"labels": {"a": "1"}, "annotations": {"kube-fence.io/intent-id": "x"}}}`,
			new:  `{"metadata": {"labels": {"a": "2"}, "annotations": {"kube-fence.io/intent-id": "y", "team": "z"}}}`,
			want: []string{"metadata.annotations.team", "metadata.labels.a"},
		},
		{
			name: "removed subtree reported per leaf",
			old:  `{"spec": {"strategy": {"type": "RollingUpdate", "rollingUpdate": {"maxSurge": 1}}}}`,
			new:  `{"spec": {}}`,
			want: []string{"spec.strategy.rollingUpdate.maxSurge", "spec.strategy.type"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, c := range Diff(obj(t, tt.old), obj(t, tt.new)) {
				got = append(got, c.Path.String())
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Diff paths = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagedFieldsFlatten(t *testing.T) {
	o := obj(t, `{"metadata": {`+hpaManaged+`}}`)
	entries := managedFields(o)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	var paths []string
	for _, e := range entries {
		for _, p := range e.Paths {
			paths = append(paths, e.Manager+"/"+e.Subresource+":"+p.String())
		}
	}
	sort.Strings(paths)
	want := []string{
		"kube-controller-manager/scale:spec.replicas",
		"kubectl-client-side-apply/:spec.template.spec.containers[name=app].image",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestOverlaps(t *testing.T) {
	p := func(s string) Path { return Path(strings.Split(s, "|")) }
	tests := []struct {
		a, b string
		want bool
	}{
		{"spec|replicas", "spec|replicas", true},
		{"spec|template", "spec|template|spec|containers", true},
		{"spec|containers|[name=a]|image", "spec|containers", true},
		{"spec|containers|[*]|image", "spec|containers|[name=a]|image", true},
		{"spec|containers|[name=a]|image", "spec|containers|[name=b]|image", false},
		{"spec|replicas", "spec|paused", false},
		{"spec|[*]", "spec|replicas", false},
	}
	for _, tt := range tests {
		if got := overlaps(p(tt.a), p(tt.b)); got != tt.want {
			t.Errorf("overlaps(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestManagerMatch(t *testing.T) {
	tests := []struct {
		m    ManagerMatch
		e    managedEntry
		want bool
	}{
		{ManagerMatch{Manager: "kube-controller-manager", Subresource: "scale"}, managedEntry{Manager: "kube-controller-manager", Subresource: "scale"}, true},
		{ManagerMatch{Manager: "kube-controller-manager", Subresource: "scale"}, managedEntry{Manager: "kube-controller-manager"}, false},
		{ManagerMatch{Manager: "argocd*"}, managedEntry{Manager: "argocd-application-controller"}, true},
		{ManagerMatch{Manager: "helm-controller"}, managedEntry{Manager: "helm"}, false},
	}
	for _, tt := range tests {
		if got := tt.m.matches(tt.e); got != tt.want {
			t.Errorf("%+v matches %+v = %v, want %v", tt.m, tt.e, got, tt.want)
		}
	}
}

func TestIntentKey(t *testing.T) {
	base := Request{Operation: OpDelete, Resource: podGV, Namespace: "shop", Name: "db-0"}
	same := base
	same.User = human
	otherName := base
	otherName.Name = "db-1"
	otherNS := base
	otherNS.Namespace = "prod"

	k := IntentKey(base, nil)
	if !strings.HasPrefix(k, "sha256:") {
		t.Fatalf("key %q has no hash prefix", k)
	}
	if IntentKey(same, nil) != k {
		t.Error("key depends on requester")
	}
	if IntentKey(otherName, nil) == k || IntentKey(otherNS, nil) == k {
		t.Error("key ignores target identity")
	}

	withAnn := Request{Operation: OpUpdate, Object: obj(t, `{"metadata": {"annotations": {"kube-fence.io/intent-id": "abc"}}}`)}
	if got := IntentKey(withAnn, nil); got != "intent:abc" {
		t.Errorf("annotated key = %q, want intent:abc", got)
	}
	delWithStale := base
	delWithStale.Object = withAnn.Object
	if IntentKey(delWithStale, nil) != k {
		t.Error("delete should ignore intent annotations")
	}
}

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string
		check   func(*testing.T, Config)
	}{
		{name: "empty uses defaults", in: "", check: func(t *testing.T, c Config) {
			if c.Mode != ModeEnforce || c.Dedupe.Window.Duration != 10*time.Minute || len(c.Ownership.ProtectedManagers) == 0 {
				t.Fatalf("unexpected defaults: %+v", c)
			}
		}},
		{name: "yaml overrides", in: `
mode: audit
agents:
  serviceAccounts: ["agents/*"]
dedupe:
  window: 2m
limits:
  default: {maxDeletes: 3, window: 5m}
  namespaces:
    prod: {maxReplicaDelta: 1}
`, check: func(t *testing.T, c Config) {
			if c.Mode != ModeAudit || c.Dedupe.Window.Duration != 2*time.Minute {
				t.Fatalf("got %+v", c)
			}
			l := c.limitsFor("prod")
			if l.MaxDeletes != 3 || l.MaxReplicaDelta != 1 || l.Window.Duration != 5*time.Minute {
				t.Fatalf("prod limits = %+v", l)
			}
		}},
		{name: "bad mode", in: "mode: yolo", wantErr: "mode must be"},
		{name: "bad duration", in: "dedupe: {window: soon}", wantErr: "invalid duration"},
		{name: "unknown field", in: "agnets: {}", wantErr: "unknown field"},
		{name: "bad service account", in: `agents: {serviceAccounts: ["remediator"]}`, wantErr: "namespace/name"},
		{name: "deletes without window", in: "limits: {default: {maxDeletes: 1, window: 0s}}", wantErr: "needs a window"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseConfig([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}
