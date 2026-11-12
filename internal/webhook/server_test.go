package webhook

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ranadhir-rikkala/kube-fence/pkg/ledger"
	"github.com/ranadhir-rikkala/kube-fence/pkg/policy"
)

var fixedNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

const agentUser = "system:serviceaccount:agents:remediator"

func newTestServer(t *testing.T, mode policy.Mode) *httptest.Server {
	t.Helper()
	cfg := policy.DefaultConfig()
	cfg.Mode = mode
	cfg.Agents.ServiceAccounts = []string{"agents/*"}
	e := policy.NewEngine(cfg, ledger.NewMemory(time.Hour), func() time.Time { return fixedNow })
	s := NewServer(e, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return fixedNow })
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func review(uid, user string, op admissionv1.Operation, resource string, object, old string) admissionv1.AdmissionReview {
	return admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID(uid),
			Operation: op,
			UserInfo:  authnv1.UserInfo{Username: user},
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: resource},
			Namespace: "shop",
			Name:      "db-0",
			Object:    raw(object),
			OldObject: raw(old),
		},
	}
}

func raw(s string) runtime.RawExtension {
	if s == "" {
		return runtime.RawExtension{}
	}
	return runtime.RawExtension{Raw: []byte(s)}
}

func post(t *testing.T, url string, ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	t.Helper()
	body, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out admissionv1.AdmissionReview
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Response == nil {
		t.Fatal("no response in review")
	}
	if out.APIVersion != "admission.k8s.io/v1" || out.Kind != "AdmissionReview" {
		t.Fatalf("type meta = %+v", out.TypeMeta)
	}
	return out.Response
}

func TestValidate(t *testing.T) {
	pod := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"db-0","namespace":"shop"}}`
	tests := []struct {
		name        string
		mode        policy.Mode
		reviews     []admissionv1.AdmissionReview
		wantAllowed []bool
		wantWarn    []bool
		wantMsg     string
	}{
		{
			name: "enforce denies duplicate delete",
			mode: policy.ModeEnforce,
			reviews: []admissionv1.AdmissionReview{
				review("u1", agentUser, admissionv1.Delete, "pods", "", pod),
				review("u2", agentUser, admissionv1.Delete, "pods", "", pod),
			},
			wantAllowed: []bool{true, false},
			wantWarn:    []bool{false, false},
			wantMsg:     "duplicate irreversible effect",
		},
		{
			name: "audit allows duplicate with warning",
			mode: policy.ModeAudit,
			reviews: []admissionv1.AdmissionReview{
				review("u1", agentUser, admissionv1.Delete, "pods", "", pod),
				review("u2", agentUser, admissionv1.Delete, "pods", "", pod),
			},
			wantAllowed: []bool{true, true},
			wantWarn:    []bool{false, true},
		},
		{
			name: "humans are not tracked",
			mode: policy.ModeEnforce,
			reviews: []admissionv1.AdmissionReview{
				review("u1", "alice", admissionv1.Delete, "pods", "", pod),
				review("u2", "alice", admissionv1.Delete, "pods", "", pod),
			},
			wantAllowed: []bool{true, true},
			wantWarn:    []bool{false, false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, tt.mode)
			var last *admissionv1.AdmissionResponse
			for i, ar := range tt.reviews {
				last = post(t, ts.URL+"/validate", ar)
				if last.UID != ar.Request.UID {
					t.Fatalf("review %d: uid = %q, want %q", i, last.UID, ar.Request.UID)
				}
				if last.Allowed != tt.wantAllowed[i] {
					t.Fatalf("review %d: allowed = %v, want %v", i, last.Allowed, tt.wantAllowed[i])
				}
				if got := len(last.Warnings) > 0; got != tt.wantWarn[i] {
					t.Fatalf("review %d: warnings = %v", i, last.Warnings)
				}
			}
			if tt.wantMsg != "" {
				if last.Result == nil || last.Result.Code != http.StatusForbidden || !strings.Contains(last.Result.Message, tt.wantMsg) {
					t.Fatalf("result = %+v, want 403 containing %q", last.Result, tt.wantMsg)
				}
			}
		})
	}
}

func TestMutate(t *testing.T) {
	ts := newTestServer(t, policy.ModeEnforce)
	tests := []struct {
		name      string
		ar        admissionv1.AdmissionReview
		wantPatch string
	}{
		{
			name:      "agent create without annotations adds the map",
			ar:        review("m1", agentUser, admissionv1.Create, "configmaps", `{"metadata":{"name":"c"},"data":{"a":"1"}}`, ""),
			wantPatch: `"path":"/metadata/annotations","value":{"kube-fence.io/last-agent-write":`,
		},
		{
			name:      "agent update escapes the annotation key",
			ar:        review("m2", agentUser, admissionv1.Update, "configmaps", `{"metadata":{"name":"c","annotations":{"x":"y"}},"data":{"a":"2"}}`, `{"metadata":{"name":"c","annotations":{"x":"y"}},"data":{"a":"1"}}`),
			wantPatch: `"path":"/metadata/annotations/kube-fence.io~1last-agent-write"`,
		},
		{
			name: "no-op update is not stamped",
			ar:   review("m3", agentUser, admissionv1.Update, "configmaps", `{"metadata":{"name":"c"},"data":{"a":"1"}}`, `{"metadata":{"name":"c"},"data":{"a":"1"}}`),
		},
		{
			name: "human writes are not stamped",
			ar:   review("m4", "alice", admissionv1.Create, "configmaps", `{"metadata":{"name":"c"}}`, ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := post(t, ts.URL+"/mutate", tt.ar)
			if !resp.Allowed {
				t.Fatalf("mutate denied: %+v", resp.Result)
			}
			if tt.wantPatch == "" {
				if len(resp.Patch) != 0 {
					t.Fatalf("unexpected patch %s", resp.Patch)
				}
				return
			}
			if resp.PatchType == nil || *resp.PatchType != admissionv1.PatchTypeJSONPatch {
				t.Fatalf("patch type = %v", resp.PatchType)
			}
			if !strings.Contains(string(resp.Patch), tt.wantPatch) {
				t.Fatalf("patch %s does not contain %s", resp.Patch, tt.wantPatch)
			}
			if !strings.Contains(string(resp.Patch), `\"at\":\"2026-11-01T12:00:00Z\"`) {
				t.Fatalf("patch %s missing timestamp", resp.Patch)
			}
		})
	}
}

func TestBadRequests(t *testing.T) {
	ts := newTestServer(t, policy.ModeEnforce)
	tests := []struct {
		name        string
		contentType string
		body        string
		want        int
	}{
		{"wrong content type", "text/plain", "{}", http.StatusUnsupportedMediaType},
		{"malformed json", "application/json", "{", http.StatusBadRequest},
		{"missing request", "application/json", `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/validate", tt.contentType, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}
