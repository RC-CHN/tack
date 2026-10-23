package control

import (
	"net/http"
	p "tack.local/management/protocol"
)

// Reject excess work before decoding bodies or allocating output buffers.
// Upload and history pressure cannot consume the control request allowance.
func takeSlot(w http.ResponseWriter, slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		failure(w, 503, "request capacity reached; retry later")
		return false
	}
}

func validNodeInfo(n p.Node) bool {
	if len(n.Hostname) > 255 || len(n.OS) > 64 || len(n.Arch) > 64 || len(n.StatePersistence) > 32 || len(n.Modes) > 16 {
		return false
	}
	for _, mode := range n.Modes {
		if len(mode) > 32 {
			return false
		}
	}
	if n.Metrics != nil {
		m := n.Metrics.Runtime
		if len(m.Control.LastError) > 64 || len(m.Upload.LastError) > 64 {
			return false
		}
	}
	return true
}
