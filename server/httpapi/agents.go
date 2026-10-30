package httpapi

// Assistant REST: owner-scoped CRUD. Status stays disabled in P0 — the
// endpoints exist to freeze config + approval surfaces.
//
// 助理 REST：属主维度 CRUD。P0 状态恒为 disabled。

import (
	"encoding/json"
	"errors"
	"net/http"
	"wittychat/core/agents"
	"wittychat/core/audit"

	"github.com/google/uuid"
)

func registerAgents(mux *http.ServeMux, d *Deps) {
	mux.HandleFunc("GET /api/v1/assistants", d.agentList)
	mux.HandleFunc("POST /api/v1/assistants", d.agentCreate)
	mux.HandleFunc("PUT /api/v1/assistants/{id}", d.agentUpdate)
	mux.HandleFunc("DELETE /api/v1/assistants/{id}", d.agentDelete)
	// Approval placeholder endpoints — UI lists pending, approve/reject stubbed.
	mux.HandleFunc("GET /api/v1/approvals", d.approvalList)
	mux.HandleFunc("POST /api/v1/approvals/{id}/decide", d.approvalDecide)
}

func (d *Deps) agentList(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	as, err := d.Agents.List(r.Context(), uid)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"assistants": as})
}

func (d *Deps) agentCreate(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var u agents.Upsert
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a, err := d.Agents.Create(r.Context(), uid, u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.Audit.Log(r.Context(), &uid, audit.ActorUser, "assistant.create", a.Name, nil, nil)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(a)
}

func (d *Deps) agentUpdate(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, _ := uuid.Parse(r.PathValue("id"))
	var u agents.Upsert
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a, err := d.Agents.Update(r.Context(), uid, id, u)
	if errors.Is(err, agents.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.Audit.Log(r.Context(), &uid, audit.ActorUser, "assistant.update", a.Name, nil, nil)
	_ = json.NewEncoder(w).Encode(a)
}

func (d *Deps) agentDelete(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id, _ := uuid.Parse(r.PathValue("id"))
	if err := d.Agents.Delete(r.Context(), uid, id); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	d.Audit.Log(r.Context(), &uid, audit.ActorUser, "assistant.delete", id.String(), nil, nil)
	w.WriteHeader(http.StatusNoContent)
}

// Approval queue lives in bg_tasks payloads until a dedicated table lands;
// P0 exposes the endpoints as a stable surface returning empty/stubbed.
func (d *Deps) approvalList(w http.ResponseWriter, r *http.Request) {
	if _, err := d.bearerUser(r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"approvals": []any{}})
}

func (d *Deps) approvalDecide(w http.ResponseWriter, r *http.Request) {
	if _, err := d.bearerUser(r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.Error(w, "approval runtime lands post-P0 / 审批运行时后置", http.StatusNotImplemented)
}
