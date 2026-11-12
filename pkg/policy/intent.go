package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// IntentKey returns the idempotency key for a write. An explicit
// kube-fence.io/intent-id annotation wins; otherwise the key is a hash of the
// operation, target, and the field-level change set. The requester is not part
// of the key, so two agents performing the same effect collide on purpose.
func IntentKey(req Request, changes []Change) string {
	if req.Operation != OpDelete {
		if id := annotation(req.Object, AnnotationIntentID); id != "" {
			return "intent:" + id
		}
	}
	type change struct {
		Path string `json:"p"`
		New  any    `json:"v"`
	}
	payload := struct {
		Op          Operation `json:"op"`
		Resource    string    `json:"gvr"`
		SubResource string    `json:"sub,omitempty"`
		Namespace   string    `json:"ns,omitempty"`
		Name        string    `json:"name,omitempty"`
		Changes     []change  `json:"changes,omitempty"`
	}{
		Op:          req.Operation,
		Resource:    req.Resource.String(),
		SubResource: req.SubResource,
		Namespace:   req.Namespace,
		Name:        req.Name,
	}
	for _, c := range changes {
		payload.Changes = append(payload.Changes, change{Path: c.Path.String(), New: c.New})
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:12])
}

func annotation(obj map[string]any, key string) string {
	md, _ := obj["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	v, _ := ann[key].(string)
	return v
}
