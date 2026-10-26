package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"

	p "tack.local/management/protocol"
)

type storedOperator struct {
	p.Operator
	Digest string `json:"digest"`
}
type principal struct {
	id                   string
	admin, read, execute bool
	nodes                map[string]bool
}
type principalKey struct{}

func principalFor(r *http.Request) principal {
	if v, ok := r.Context().Value(principalKey{}).(principal); ok {
		return v
	}
	return principal{id: "admin", admin: true, read: true, execute: true}
}
func (a principal) allows(node string) bool { return a.admin || a.nodes[node] }
func tokenDigest(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func (s *Server) authenticate(token string) (principal, bool) {
	if equalToken(token, s.config.AdminToken) {
		return principal{id: "admin", admin: true, read: true, execute: true}, true
	}
	digest := tokenDigest(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, op := range s.operators {
		if !op.Disabled && equalToken(digest, op.Digest) {
			a := principal{id: op.ID, read: op.Read, execute: op.Execute, nodes: make(map[string]bool, len(op.NodeIDs))}
			for _, n := range op.NodeIDs {
				a.nodes[n] = true
			}
			return a, true
		}
	}
	return principal{}, false
}
func (s *Server) openOperators() error {
	s.operators = map[string]storedOperator{}
	if err := p.ReadJSON(s.path+".operators.json", &s.operators); err != nil && !os.IsNotExist(err) {
		return err
	}
	if s.operators == nil || len(s.operators) > 256 {
		return errors.New("invalid operator store")
	}
	seen := map[string]bool{tokenDigest(s.config.AdminToken): true}
	for _, n := range s.config.Nodes {
		seen[tokenDigest(n.Token)] = true
	}
	for id, op := range s.operators {
		digest, err := hex.DecodeString(op.Digest)
		if id != op.ID || !p.ValidID(id) || id == "admin" || err != nil || len(digest) != 32 || hex.EncodeToString(digest) != op.Digest || seen[op.Digest] || op.Token != "" || len(op.NodeIDs) == 0 || len(op.NodeIDs) > 1024 || (!op.Read && !op.Execute) {
			return errors.New("invalid stored operator")
		}
		seen[op.Digest] = true
		nodes := map[string]bool{}
		for _, node := range op.NodeIDs {
			if !p.ValidID(node) || nodes[node] {
				return errors.New("invalid stored operator scope")
			}
			nodes[node] = true
		}
	}
	return nil
}
func (s *Server) operatorAPI(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/v1/operators" && !strings.HasPrefix(r.URL.Path, "/v1/operators/") {
		return false
	}
	if !principalFor(r).admin {
		failure(w, 403, "administrator permission required")
		return true
	}
	s.tx.Lock()
	defer s.tx.Unlock()
	if r.Method == "GET" && r.URL.Path == "/v1/operators" {
		list := make([]p.Operator, 0, len(s.operators))
		for _, op := range s.operators {
			list = append(list, op.Operator)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		respond(w, 200, list)
		return true
	}
	next := make(map[string]storedOperator, len(s.operators)+1)
	for id, op := range s.operators {
		next[id] = op
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/operators":
		var op p.Operator
		if !decode(w, r, &op) {
			return true
		}
		if !p.ValidID(op.ID) || op.ID == "admin" || len(op.Token) < 32 || len(op.Token) > 4096 || len(op.NodeIDs) == 0 || len(op.NodeIDs) > 1024 || (!op.Read && !op.Execute) {
			failure(w, 400, "valid ID, 32..4096 character token, node whitelist and read/execute permission required")
			return true
		}
		seen := map[string]bool{}
		for _, n := range op.NodeIDs {
			if !s.nodeExists(n) || seen[n] {
				failure(w, 400, "invalid or duplicate node whitelist entry")
				return true
			}
			seen[n] = true
		}
		digest := tokenDigest(op.Token)
		if equalToken(op.Token, s.config.AdminToken) {
			failure(w, 409, "credential must be unique")
			return true
		}
		for _, n := range s.config.Nodes {
			if equalToken(op.Token, n.Token) {
				failure(w, 409, "credential must be unique")
				return true
			}
		}
		for id, saved := range next {
			if id != op.ID && equalToken(digest, saved.Digest) {
				failure(w, 409, "credential must be unique")
				return true
			}
		}
		op.Token = ""
		next[op.ID] = storedOperator{Operator: op, Digest: digest}
		if len(next) > 256 {
			failure(w, 409, "operator capacity reached")
			return true
		}
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/operators/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/operators/")
		if !p.ValidID(id) {
			failure(w, 400, "invalid operator ID")
			return true
		}
		// Preserve the stable ID across rotation and revocation, including its
		// request namespace. Re-granting the same ID cannot duplicate old requests.
		op, ok := next[id]
		if !ok {
			failure(w, 404, "operator not found")
			return true
		}
		op.Disabled = true
		next[id] = op
	default:
		failure(w, 405, "unsupported operator operation")
		return true
	}
	if err := s.writeJSON(s.path+".operators.json", next); err != nil {
		failure(w, 500, "cannot persist operator credentials")
		return true
	}
	s.mu.Lock()
	s.operators = next
	s.mu.Unlock()
	respond(w, 200, map[string]bool{"updated": true})
	return true
}
func withPrincipal(r *http.Request, a principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, a))
}
