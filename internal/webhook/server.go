// Package webhook adapts Kubernetes AdmissionReview requests to the policy
// engine.
package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ranadhir-rikkala/kube-fence/pkg/policy"
)

const maxBodyBytes = 8 << 20

type Server struct {
	engine *policy.Engine
	log    *slog.Logger
	now    func() time.Time
}

func NewServer(e *policy.Engine, log *slog.Logger, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{engine: e, log: log, now: now}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /validate", s.review(s.validate))
	mux.HandleFunc("POST /mutate", s.review(s.mutate))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	return mux
}

type reviewFunc func(*admissionv1.AdmissionRequest, policy.Request) *admissionv1.AdmissionResponse

func (s *Server) review(fn reviewFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
			return
		}
		var review admissionv1.AdmissionReview
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&review); err != nil {
			http.Error(w, "decode AdmissionReview: "+err.Error(), http.StatusBadRequest)
			return
		}
		if review.Request == nil {
			http.Error(w, "AdmissionReview has no request", http.StatusBadRequest)
			return
		}
		var resp *admissionv1.AdmissionResponse
		req, err := toPolicyRequest(review.Request)
		if err != nil {
			resp = &admissionv1.AdmissionResponse{Result: &metav1.Status{
				Code: http.StatusBadRequest, Message: err.Error(), Reason: metav1.StatusReasonBadRequest,
			}}
		} else {
			resp = fn(review.Request, req)
		}
		resp.UID = review.Request.UID
		out := admissionv1.AdmissionReview{TypeMeta: review.TypeMeta, Response: resp}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			s.log.Error("write response", "err", err)
		}
	}
}

func (s *Server) validate(ar *admissionv1.AdmissionRequest, req policy.Request) *admissionv1.AdmissionResponse {
	d := s.engine.Evaluate(req)
	if d.Agent {
		s.logDecision(req, d)
	}
	resp := &admissionv1.AdmissionResponse{Allowed: d.Allowed}
	switch {
	case !d.Allowed:
		resp.Result = &metav1.Status{
			Code:    http.StatusForbidden,
			Reason:  metav1.StatusReasonForbidden,
			Message: d.Message(),
		}
	case d.Audit:
		resp.Warnings = []string{d.Message()}
	}
	return resp
}

func (s *Server) logDecision(req policy.Request, d policy.Decision) {
	attrs := []any{
		"uid", req.UID,
		"agent", req.User.Username,
		"operation", req.Operation,
		"resource", req.Resource.String(),
		"subresource", req.SubResource,
		"namespace", req.Namespace,
		"name", req.Name,
		"intent", d.IntentKey,
		"allowed", d.Allowed,
		"recorded", d.Recorded,
		"dryRun", req.DryRun,
	}
	switch {
	case d.NoOp:
		s.log.Info("agent write is a no-op", attrs...)
	case len(d.Violations) == 0:
		s.log.Info("agent write allowed", attrs...)
	case d.Audit:
		s.log.Warn("agent write would be denied (audit mode)", append(attrs, "violations", d.Violations)...)
	default:
		s.log.Warn("agent write denied", append(attrs, "violations", d.Violations)...)
	}
}

type lastWrite struct {
	Agent  string `json:"agent"`
	Intent string `json:"intent"`
	At     string `json:"at"`
}

// mutate stamps provenance onto objects that agents create or change. It never
// denies; enforcement belongs to the validating webhook, which runs after all
// mutations are final.
func (s *Server) mutate(ar *admissionv1.AdmissionRequest, req policy.Request) *admissionv1.AdmissionResponse {
	resp := &admissionv1.AdmissionResponse{Allowed: true}
	if req.SubResource != "" || req.Object == nil || !s.engine.IsAgent(req.User) {
		return resp
	}
	var changes []policy.Change
	switch req.Operation {
	case policy.OpCreate:
		changes = policy.Diff(nil, req.Object)
	case policy.OpUpdate:
		changes = policy.Diff(req.OldObject, req.Object)
		if len(changes) == 0 {
			return resp
		}
	default:
		return resp
	}
	value, err := json.Marshal(lastWrite{
		Agent:  req.User.Username,
		Intent: policy.IntentKey(req, changes),
		At:     s.now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return resp
	}
	patch, err := annotationPatch(req.Object, policy.AnnotationLastAgentWrite, string(value))
	if err != nil {
		s.log.Error("build patch", "err", err)
		return resp
	}
	pt := admissionv1.PatchTypeJSONPatch
	resp.Patch = patch
	resp.PatchType = &pt
	return resp
}

func annotationPatch(obj map[string]any, key, value string) ([]byte, error) {
	type op struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	md, _ := obj["metadata"].(map[string]any)
	if _, ok := md["annotations"].(map[string]any); !ok {
		return json.Marshal([]op{{Op: "add", Path: "/metadata/annotations", Value: map[string]string{key: value}}})
	}
	return json.Marshal([]op{{Op: "add", Path: "/metadata/annotations/" + escapePointer(key), Value: value}})
}

func escapePointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func toPolicyRequest(ar *admissionv1.AdmissionRequest) (policy.Request, error) {
	req := policy.Request{
		UID:         string(ar.UID),
		Operation:   policy.Operation(ar.Operation),
		User:        policy.UserInfo{Username: ar.UserInfo.Username, Groups: ar.UserInfo.Groups},
		Resource:    policy.GVR{Group: ar.Resource.Group, Version: ar.Resource.Version, Resource: ar.Resource.Resource},
		SubResource: ar.SubResource,
		Namespace:   ar.Namespace,
		Name:        ar.Name,
		DryRun:      ar.DryRun != nil && *ar.DryRun,
	}
	var err error
	if req.Object, err = decodeObject(ar.Object.Raw); err != nil {
		return req, fmt.Errorf("decode object: %w", err)
	}
	if req.OldObject, err = decodeObject(ar.OldObject.Raw); err != nil {
		return req, fmt.Errorf("decode oldObject: %w", err)
	}
	return req, nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}
